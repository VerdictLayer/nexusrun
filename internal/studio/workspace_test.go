package studio

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/verdictlayer/nexusrun/internal/server"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func newWorkspaceEnv(t *testing.T) (*env, string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	p := &project{shared: &shared{store: st, token: token, ws: ws, projects: map[string]*project{}, code: "0123456789abcdef0123456789abcdef", codeBy: time.Now().Add(time.Minute)}}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = server.ProtectExcept(p.routes(), ts.Listener.Addr().String(), token, "/api/exchange")
	ts.Start()
	t.Cleanup(ts.Close)
	return &env{t: t, root: ws, srv: ts, p: p}, ws
}

func names(out []byte) []string {
	var r struct {
		Workspace bool
		Projects  []struct{ Name string }
	}
	json.Unmarshal(out, &r)
	var n []string
	for _, p := range r.Projects {
		n = append(n, p.Name)
	}
	return n
}

func TestWorkspaceListsAndCreatesProjects(t *testing.T) {
	e, ws := newWorkspaceEnv(t)
	code, out := e.do("GET", "/api/workspace", nil)
	if code != 200 || len(names(out)) != 0 || !strings.Contains(string(out), `"workspace":true`) {
		t.Fatalf("empty workspace: %d %s", code, out)
	}
	if c, out := e.do("POST", "/api/workspace/new", map[string]string{"name": "alpha"}); c != 201 {
		t.Fatalf("create: %d %s", c, out)
	}
	if st, err := os.Stat(filepath.Join(ws, "alpha")); err != nil || !st.IsDir() {
		t.Fatal("the folder was not created")
	}
	if c, _ := e.do("POST", "/api/workspace/new", map[string]string{"name": "alpha"}); c != 409 {
		t.Errorf("a duplicate name: %d", c)
	}
	for _, bad := range []string{"", ".hidden", "../escape", "a/b", `a\b`, "-x", strings.Repeat("a", 65), "sp ace"} {
		if c, _ := e.do("POST", "/api/workspace/new", map[string]string{"name": bad}); c != 400 {
			t.Errorf("name %q: status %d, want 400", bad, c)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape")); err == nil {
		t.Error("a name escaped the workspace")
	}
	_, out = e.do("GET", "/api/workspace", nil)
	if got := names(out); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("listed %v", got)
	}
}

func TestWorkspaceProjectsAreSeparateAndNamedByHeaderOnly(t *testing.T) {
	e, ws := newWorkspaceEnv(t)
	e.do("POST", "/api/workspace/new", map[string]string{"name": "alpha"})
	e.do("POST", "/api/workspace/new", map[string]string{"name": "beta"})
	alpha := []string{projectHeader, "alpha"}

	if c, _ := e.do("GET", "/api/project", nil); c != 404 {
		t.Errorf("a project route without a project: %d", c)
	}
	for _, bad := range []string{"nope", "../alpha", "alpha/..", "", ".", ".."} {
		if c, _ := e.do("GET", "/api/project", nil, projectHeader, bad); c != 404 {
			t.Errorf("project %q: %d, want 404", bad, c)
		}
	}
	if c, out := e.do("PUT", "/api/workflow", map[string]string{"yaml": "name: a\n"}, alpha...); c != 200 {
		t.Fatalf("save: %d %s", c, out)
	}
	if c, _ := e.do("PUT", "/api/unit?agent=writer", map[string]any{"prompt": "hi", "model": "m"}, alpha...); c != 200 {
		t.Fatal("unit")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "alpha", "nexus-compose.yaml")); string(b) != "name: a\n" {
		t.Error("alpha's file was not written in alpha")
	}
	if _, err := os.Stat(filepath.Join(ws, "beta", "nexus-compose.yaml")); err == nil {
		t.Error("beta must be untouched")
	}
	if _, out := e.do("GET", "/api/unit?agent=writer", nil, projectHeader, "beta"); strings.Contains(string(out), `"exists":true`) {
		t.Errorf("beta must not see alpha's unit: %s", out)
	}
	_, out := e.do("GET", "/api/workspace", nil)
	var r struct{ Projects []projectEntry }
	json.Unmarshal(out, &r)
	if len(r.Projects) != 2 || r.Projects[0].Name != "alpha" || !r.Projects[0].HasWorkflow {
		t.Errorf("the most recently changed project comes first and shows it has a workflow: %+v", r.Projects)
	}
}

func TestWorkspaceIgnoresSymbolicLinks(t *testing.T) {
	e, ws := newWorkspaceEnv(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "linked")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, out := e.do("GET", "/api/workspace", nil)
	if len(names(out)) != 0 {
		t.Errorf("a symlink is not a project: %v", names(out))
	}
	if c, _ := e.do("PUT", "/api/workflow", map[string]string{"yaml": "x"}, projectHeader, "linked"); c != 404 {
		t.Errorf("writing through a symlinked project: %d", c)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a write escaped the workspace: %v", entries)
	}
}

func TestSingleFolderModeHasNoWorkspace(t *testing.T) {
	e := newEnv(t)
	_, out := e.do("GET", "/api/workspace", nil)
	if !strings.Contains(string(out), `"workspace":false`) {
		t.Errorf("got %s", out)
	}
	if c, _ := e.do("POST", "/api/workspace/new", map[string]string{"name": "x"}); c != 400 {
		t.Errorf("creating a project outside a workspace: %d", c)
	}
	// The project header is ignored: there is only one folder.
	if c, _ := e.do("GET", "/api/project", nil, projectHeader, "../../etc"); c != 200 {
		t.Errorf("single mode: %d", c)
	}
}

func TestStudioStopsWhenThePageIsGoneForGood(t *testing.T) {
	t0 := time.Now()
	var l liveness
	l.start(t0)
	if l.expired(t0.Add(time.Minute)) {
		t.Error("it must wait a while for the page to open")
	}
	if !l.expired(t0.Add(neverOpened + time.Second)) {
		t.Error("nobody ever opened it")
	}

	l.ping(t0)
	if l.expired(t0.Add(time.Minute)) {
		t.Error("a page that pinged a minute ago is open")
	}
	l.closed(t0.Add(time.Minute))
	if l.expired(t0.Add(time.Minute + closeGrace/2)) {
		t.Error("a closing page gets a moment in case it is only reloading")
	}
	l.ping(t0.Add(time.Minute + closeGrace/2)) // the reloaded page
	if l.expired(t0.Add(time.Minute + 2*closeGrace)) {
		t.Error("a reload must not stop the editor")
	}
	l.closed(t0.Add(2 * time.Minute))
	if !l.expired(t0.Add(2*time.Minute + closeGrace + time.Second)) {
		t.Error("a page that closed and did not return ends the editor")
	}

	var crashed liveness
	crashed.start(t0)
	crashed.ping(t0)
	if !crashed.expired(t0.Add(silence + time.Second)) {
		t.Error("a window that stopped pinging without saying so ends the editor")
	}
}
