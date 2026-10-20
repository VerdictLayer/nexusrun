package studio

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/verdictlayer/nexusrun/internal/workflow"
)

// A workspace is a folder of projects. The page picks one by name in a header;
// the name is looked up as a direct child of the workspace and nothing else, so
// there is no way to name a path elsewhere.

var projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const (
	projectHeader = "X-Studio-Project"
	maxProjects   = 500
)

// scoped runs a handler against the project the request names. With a single
// folder that is always the folder; in a workspace it is the named child.
func (p *project) scoped(fn func(*project, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proj, err := p.resolve(r)
		if err != nil {
			fail(w, http.StatusNotFound, "%v", err)
			return
		}
		fn(proj, w, r)
	}
}

func (p *project) resolve(r *http.Request) (*project, error) {
	if p.ws == "" {
		return p, nil
	}
	name := r.Header.Get(projectHeader)
	if !projectName.MatchString(name) {
		return nil, errors.New("choose a project first")
	}
	dir := filepath.Join(p.ws, name)
	// Lstat, so a symbolic link in the workspace is not a project.
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("there is no project called " + name)
	}
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if c, ok := p.projects[name]; ok {
		return c, nil
	}
	c := &project{root: dir, shared: p.shared}
	p.projects[name] = c
	return c, nil
}

type projectEntry struct {
	Name        string    `json:"name"`
	HasWorkflow bool      `json:"has_workflow"`
	Modified    time.Time `json:"modified"`
}

func (p *project) workspaceInfo(w http.ResponseWriter, r *http.Request) {
	if p.ws == "" {
		writeJSON(w, http.StatusOK, map[string]any{"workspace": false})
		return
	}
	entries, err := os.ReadDir(p.ws)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	list := []projectEntry{}
	for _, e := range entries {
		if len(list) >= maxProjects {
			break
		}
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || !projectName.MatchString(e.Name()) {
			continue
		}
		pe := projectEntry{Name: e.Name()}
		if info, err := e.Info(); err == nil {
			pe.Modified = info.ModTime()
		}
		if info, err := os.Lstat(filepath.Join(p.ws, e.Name(), workflow.FileName)); err == nil && info.Mode().IsRegular() {
			pe.HasWorkflow, pe.Modified = true, info.ModTime()
		}
		list = append(list, pe)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Modified.After(list[j].Modified) })
	writeJSON(w, http.StatusOK, map[string]any{"workspace": true, "root": p.ws, "projects": list})
}

func (p *project) newProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if p.ws == "" {
		fail(w, http.StatusBadRequest, "this editor was opened on one folder, not a workspace")
		return
	}
	if !projectName.MatchString(in.Name) {
		fail(w, http.StatusBadRequest, "a name uses letters, digits, dots, dashes and underscores, and starts with a letter or digit")
		return
	}
	if err := os.Mkdir(filepath.Join(p.ws, in.Name), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			fail(w, http.StatusConflict, "there is already a project called %s", in.Name)
			return
		}
		fail(w, http.StatusInternalServerError, "could not create it: %v", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name})
}

// ---- liveness --------------------------------------------------------------

// liveness decides when an editor with no terminal should stop: when the page
// says it is closing and does not come back, or has been silent for a long time.
// A reload sends "closing" and then pings again within the grace period.
type liveness struct {
	mu                sync.Mutex
	started, lastPing time.Time
	closing           time.Time
	pinged            bool
}

const (
	closeGrace  = 8 * time.Second // a reload comes back sooner than this
	silence     = 5 * time.Minute // a crashed window stops pinging
	neverOpened = 3 * time.Minute // nobody opened the page at all
)

func (l *liveness) start(now time.Time) { l.mu.Lock(); l.started = now; l.mu.Unlock() }
func (l *liveness) ping(now time.Time) {
	l.mu.Lock()
	l.lastPing, l.pinged, l.closing = now, true, time.Time{}
	l.mu.Unlock()
}
func (l *liveness) closed(now time.Time) { l.mu.Lock(); l.closing = now; l.mu.Unlock() }
func (l *liveness) expired(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case !l.closing.IsZero():
		return now.Sub(l.closing) > closeGrace
	case l.pinged:
		return now.Sub(l.lastPing) > silence
	default:
		return now.Sub(l.started) > neverOpened
	}
}

func (p *project) ping(w http.ResponseWriter, r *http.Request) {
	p.live.ping(time.Now())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *project) closing(w http.ResponseWriter, r *http.Request) {
	p.live.closed(time.Now())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
