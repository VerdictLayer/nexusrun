// Package studio serves the workflow editor: a canvas for building a
// nexus-compose.yaml by dragging agents, links and models together.
//
// The editor changes files; it never reimplements what they mean. Parsing,
// validation and rendering all go through the same packages the CLI uses, so
// what the canvas calls valid is exactly what `nexus compose validate` calls
// valid.
//
// It is a local server, so it follows the rule for every local server: web
// pages in the user's browser can reach loopback too. It sits behind the same
// Host/Origin guard and bearer token as the console, and it can only touch a
// short list of files inside one project folder. There is no endpoint that reads
// an arbitrary path or runs a command.
package studio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/modelscan"
	"github.com/verdictlayer/nexusrun/internal/server"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/workflow"
)

//go:embed studio.html
var page []byte

const (
	maxBody       = 1 << 20
	maxLayout     = 256 << 10
	layoutDir     = ".nexusrun"
	layoutFile    = "studio.json"
	settingsFile  = "studio.json"
	maxModelDirs  = 16
	agentsDir     = "agents"
	unitFile      = "nexus.yaml"
	backupSuffix  = ".bak"
	defaultPrefer = "cpu"
	// placeholderModel stands in for a model until one is chosen, so a draft
	// unit is a valid file that visibly has no model rather than a broken one.
	placeholderModel = "choose-a-model"
)

var agentName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// Options configures a Studio server.
type Options struct {
	// Dir is the project folder: where nexus-compose.yaml lives.
	Dir   string
	Addr  string
	Store *store.Store
	// ModelDirs are folders to search for models in addition to the ones saved
	// in settings (from flags or the environment).
	ModelDirs []string
}

type project struct {
	root      string
	store     *store.Store
	fixedDirs []string
	mu        sync.Mutex // serialises writes
	busy      atomic.Bool
	exe       string // the program that runs workflows and tests: this executable

	token string
	// code is a one-time stand-in for the token, for links that end up on a
	// command line where other accounts can read them. See exchange.
	code     string
	codeUsed bool
	codeBy   time.Time
}

// Serve runs the editor until ctx ends. onReady receives the link to open.
// Links are the ways to open the editor.
type Links struct {
	// Token carries the session token. Show it only to the person at the terminal.
	Token string
	// Code works once, within a couple of minutes, and is exchanged for the
	// token by the page. It is safe to pass to another program.
	Code string
}

const codeLifetime = 2 * time.Minute

func Serve(ctx context.Context, o Options, onReady func(Links)) error {
	if err := server.RequireLoopback(o.Addr); err != nil {
		return err
	}
	root, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a folder", o.Dir)
	}
	token, err := server.NewSessionToken()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	code, err := server.NewSessionToken()
	if err != nil {
		return err
	}
	p := &project{root: root, store: o.Store, fixedDirs: o.ModelDirs, exe: exe, token: token, code: code[:32], codeBy: time.Now().Add(codeLifetime)}

	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.Addr, err)
	}
	srv := &http.Server{
		Handler:           server.ProtectExcept(p.routes(), ln.Addr().String(), token, "/api/exchange"),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if onReady != nil {
		// Fragments are never sent to a server. The token link is for the terminal;
		// the code link is for programs that put it on a command line.
		onReady(Links{
			Token: fmt.Sprintf("http://%s/#token=%s", ln.Addr(), token),
			Code:  fmt.Sprintf("http://%s/#code=%s", ln.Addr(), p.code),
		})
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *project) routes() http.Handler {
	mux := http.NewServeMux()
	csp := contentSecurityPolicy(page)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Write(page)
	})
	mux.HandleFunc("GET /api/project", p.getProject)
	mux.HandleFunc("POST /api/parse", p.parse)
	mux.HandleFunc("POST /api/render", p.render)
	mux.HandleFunc("PUT /api/workflow", p.putWorkflow)
	mux.HandleFunc("GET /api/unit", p.getUnit)
	mux.HandleFunc("PUT /api/unit", p.putUnit)
	mux.HandleFunc("PUT /api/layout", p.putLayout)
	mux.HandleFunc("GET /api/models", p.models)
	mux.HandleFunc("PUT /api/settings", p.putSettings)
	mux.HandleFunc("POST /api/exchange", p.exchange)
	mux.HandleFunc("POST /api/run", p.run)
	mux.HandleFunc("POST /api/test-model", p.testModel)
	return mux
}

// contentSecurityPolicy allows exactly the page's own inline script and style.
// Model names and unit references come from files on disk, so the page must not
// be able to run anything injected into them.
func contentSecurityPolicy(html []byte) string {
	hash := func(re string) string {
		var out []string
		for _, m := range regexp.MustCompile(re).FindAllSubmatch(html, -1) {
			sum := sha256.Sum256(m[1])
			out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		return strings.Join(out, " ")
	}
	return "default-src 'none'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
		"script-src " + hash(`(?s)<script>(.*?)</script>`) + "; style-src " + hash(`(?s)<style>(.*?)</style>`)
}

// exchange trades the one-time code for the session token. A link with the token
// in it would be readable in the process list by other accounts on this machine
// while a browser is starting; the code is useless a moment later.
func (p *project) exchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	p.mu.Lock()
	valid := !p.codeUsed && time.Now().Before(p.codeBy) && subtle.ConstantTimeCompare([]byte(in.Code), []byte(p.code)) == 1
	if valid {
		p.codeUsed = true
	}
	p.mu.Unlock()
	if !valid {
		fail(w, http.StatusForbidden, "this link was already used or has expired; use the link printed in the terminal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": p.token})
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid request: %v", err)
		return false
	}
	return true
}

// inside resolves rel under the project root, refusing any path that passes
// through a symlink so a link planted in the folder cannot redirect a write.
func (p *project) inside(rel ...string) (string, error) {
	cur := p.root
	for _, part := range rel {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", fmt.Errorf("invalid path element %q", part)
		}
		cur = filepath.Join(cur, part)
		if info, err := os.Lstat(cur); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link", part)
		}
	}
	return cur, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".studio-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readIfExists(path string, limit int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return b, true, nil
}

// ---- project ---------------------------------------------------------------

func (p *project) settingsPath() string { return filepath.Join(p.store.Root, settingsFile) }

type settings struct {
	ModelDirs []string `json:"model_dirs"`
}

func (p *project) loadSettings() settings {
	var s settings
	if b, ok, err := readIfExists(p.settingsPath(), 64<<10); err == nil && ok {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (p *project) getProject(w http.ResponseWriter, r *http.Request) {
	path, err := p.inside(workflow.FileName)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	body, exists, err := readIfExists(path, maxBody)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	var layout json.RawMessage = json.RawMessage("{}")
	if lp, err := p.inside(layoutDir, layoutFile); err == nil {
		if b, ok, err := readIfExists(lp, maxLayout); err == nil && ok && json.Valid(b) {
			layout = b
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":          p.root,
		"name":         filepath.Base(p.root),
		"file":         workflow.FileName,
		"exists":       exists,
		"yaml":         string(body),
		"has_comments": exists && hasComments(body),
		"layout":       layout,
		"model_dirs":   p.loadSettings().ModelDirs,
		"fixed_dirs":   p.fixedDirs,
	})
}

func hasComments(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			return true
		}
	}
	return false
}

type report struct {
	Spec        *workflow.Spec        `json:"spec,omitempty"`
	YAML        string                `json:"yaml,omitempty"`
	Order       []string              `json:"order,omitempty"`
	Diagnostics []workflow.Diagnostic `json:"diagnostics"`
}

func diagnose(data []byte) report {
	spec, ds := workflow.Diagnose(data)
	if ds == nil {
		ds = []workflow.Diagnostic{}
	}
	rep := report{Spec: spec, Diagnostics: ds}
	if spec != nil && len(ds) == 0 {
		rep.Order, _ = spec.Order()
	}
	return rep
}

func (p *project) parse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		YAML string `json:"yaml"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeJSON(w, http.StatusOK, diagnose([]byte(in.YAML)))
}

// render turns the editor's model of the workflow into the file's YAML and says
// what is wrong with it, in one round trip.
func (p *project) render(w http.ResponseWriter, r *http.Request) {
	var spec workflow.Spec
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct {
		Spec *workflow.Spec `json:"spec"`
	}{&spec}); err != nil {
		fail(w, http.StatusBadRequest, "invalid workflow: %v", err)
		return
	}
	spec.APIVersion, spec.Kind = workflow.APIVersion, workflow.Kind
	// Two-space indentation, like the files `nexus compose init` writes.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&spec); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	enc.Close()
	out := buf.Bytes()
	rep := diagnose(out)
	rep.Spec, rep.YAML = nil, string(out)
	writeJSON(w, http.StatusOK, rep)
}

func (p *project) putWorkflow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		YAML string `json:"yaml"`
	}
	if !decode(w, r, &in) {
		return
	}
	path, err := p.inside(workflow.FileName)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Studio rewrites the whole file, which drops comments. Keep the original
	// next to it, once, so nothing hand-written is lost.
	if old, ok, err := readIfExists(path, maxBody); err == nil && ok && string(old) != in.YAML {
		if bak, err := p.inside(workflow.FileName + backupSuffix); err == nil {
			if _, statErr := os.Stat(bak); errors.Is(statErr, os.ErrNotExist) {
				_ = writeAtomic(bak, old)
			}
		}
	}
	if err := writeAtomic(path, []byte(in.YAML)); err != nil {
		fail(w, http.StatusInternalServerError, "could not save: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *project) putLayout(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLayout))
	if err != nil || !json.Valid(body) {
		fail(w, http.StatusBadRequest, "layout must be JSON of at most %d bytes", maxLayout)
		return
	}
	path, err := p.inside(layoutDir, layoutFile)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := writeAtomic(path, body); err != nil {
		fail(w, http.StatusInternalServerError, "could not save: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- units -----------------------------------------------------------------

func (p *project) unitPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	name := r.URL.Query().Get("agent")
	if !agentName.MatchString(name) {
		fail(w, http.StatusBadRequest, "agent must be a name like writer or fact_checker")
		return "", "", false
	}
	path, err := p.inside(agentsDir, name, unitFile)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return "", "", false
	}
	return name, path, true
}

// loadUnit reads a unit file without applying defaults, so saving it back
// changes only what the editor changed.
func loadUnit(data []byte) (*manifest.Manifest, error) {
	var m manifest.Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

type unitView struct {
	Exists   bool     `json:"exists"`
	Editable bool     `json:"editable"`
	Reason   string   `json:"reason,omitempty"`
	Ref      string   `json:"ref"`
	Prompt   string   `json:"prompt"`
	Model    string   `json:"model"`
	Context  int      `json:"context"`
	Prefer   []string `json:"prefer"`
}

func viewOf(name string, m *manifest.Manifest) unitView {
	v := unitView{Exists: true, Editable: true, Ref: "./" + agentsDir + "/" + name, Prefer: m.Hardware.Prefer}
	v.Prompt = m.Entrypoint.SystemPrompt
	switch {
	case m.Entrypoint.Type != "" && m.Entrypoint.Type != "chat":
		v.Editable, v.Reason = false, "this is a script unit; edit it in your editor"
	case len(m.Models) != 1 || m.Models[0].Auto():
		v.Editable, v.Reason = false, "this unit selects among several models; edit its models in your editor"
	}
	if len(m.Models) == 1 && !m.Models[0].Auto() {
		v.Model, v.Context = m.Models[0].Source, m.Models[0].Context
		if v.Model == placeholderModel {
			v.Model = ""
		}
	}
	return v
}

func (p *project) getUnit(w http.ResponseWriter, r *http.Request) {
	name, path, ok := p.unitPath(w, r)
	if !ok {
		return
	}
	data, exists, err := readIfExists(path, maxBody)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	if !exists {
		writeJSON(w, http.StatusOK, unitView{Ref: "./" + agentsDir + "/" + name, Editable: true, Prefer: []string{defaultPrefer}})
		return
	}
	m, err := loadUnit(data)
	if err != nil {
		writeJSON(w, http.StatusOK, unitView{Exists: true, Ref: "./" + agentsDir + "/" + name, Reason: "this unit file has problems: " + firstLine(err)})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(name, m))
}

func firstLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for _, l := range lines {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")); l != "" && !strings.HasPrefix(l, "invalid manifest") {
			return l
		}
	}
	return err.Error()
}

var unitNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

func unitName(agent string) string {
	n := strings.Trim(unitNameChars.ReplaceAllString(strings.ToLower(strings.ReplaceAll(agent, "_", "-")), "-"), "-")
	if n == "" {
		n = "agent"
	}
	return n
}

func (p *project) putUnit(w http.ResponseWriter, r *http.Request) {
	name, path, ok := p.unitPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Prompt  *string `json:"prompt"`
		Model   *string `json:"model"`
		Context *int    `json:"context"`
	}
	if !decode(w, r, &in) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var m *manifest.Manifest
	data, exists, err := readIfExists(path, maxBody)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	if exists {
		if m, err = loadUnit(data); err != nil {
			fail(w, http.StatusConflict, "this unit file has problems, so Studio will not rewrite it: %s", firstLine(err))
			return
		}
		if v := viewOf(name, m); !v.Editable {
			fail(w, http.StatusConflict, "%s", v.Reason)
			return
		}
	} else {
		m = &manifest.Manifest{
			APIVersion: manifest.APIVersion, Name: unitName(name), Version: "0.1.0",
			Models:     []manifest.Model{{ID: "main"}},
			Entrypoint: manifest.Entrypoint{Type: "chat"},
			Hardware:   manifest.Hardware{Prefer: []string{defaultPrefer}},
		}
	}
	if in.Prompt != nil {
		m.Entrypoint.SystemPrompt = *in.Prompt
	}
	if in.Model != nil {
		m.Models[0].Source = strings.TrimSpace(*in.Model)
	}
	if in.Context != nil {
		if *in.Context < 0 {
			fail(w, http.StatusBadRequest, "context cannot be negative")
			return
		}
		m.Models[0].Context = *in.Context
	}
	// A new unit with no model yet is a draft; it names the placeholder until one is chosen.
	if m.Models[0].Source == "" {
		m.Models[0].Source = placeholderModel
	}
	if err := m.Validate(); err != nil {
		fail(w, http.StatusBadRequest, "%s", firstLine(err))
		return
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := writeAtomic(path, out); err != nil {
		fail(w, http.StatusInternalServerError, "could not save: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(name, m))
}

// ---- models ----------------------------------------------------------------

func (p *project) allDirs() []string {
	dirs := append([]string{}, p.fixedDirs...)
	return append(dirs, p.loadSettings().ModelDirs...)
}

func (p *project) models(w http.ResponseWriter, r *http.Request) {
	ms := modelscan.Scan(modelscan.Options{Store: p.store, Dirs: p.allDirs()})
	if ms == nil {
		ms = []modelscan.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ms, "hardware": hardware.Detect()})
}

func (p *project) putSettings(w http.ResponseWriter, r *http.Request) {
	var in settings
	if !decode(w, r, &in) {
		return
	}
	if len(in.ModelDirs) > maxModelDirs {
		fail(w, http.StatusBadRequest, "at most %d folders", maxModelDirs)
		return
	}
	clean := []string{}
	for _, d := range in.ModelDirs {
		if !filepath.IsAbs(d) {
			fail(w, http.StatusBadRequest, "%q is not an absolute folder path", d)
			return
		}
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			fail(w, http.StatusBadRequest, "%q is not a folder", d)
			return
		}
		clean = append(clean, filepath.Clean(d))
	}
	out, _ := json.Marshal(settings{ModelDirs: clean})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := writeAtomic(p.settingsPath(), out); err != nil {
		fail(w, http.StatusInternalServerError, "could not save: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_dirs": clean})
}
