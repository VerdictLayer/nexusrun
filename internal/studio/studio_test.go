package studio

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/verdictlayer/nexusrun/internal/server"
	"github.com/verdictlayer/nexusrun/internal/store"
)

const token = "test-token-0123456789abcdef"

type env struct {
	t    *testing.T
	root string
	srv  *httptest.Server
	p    *project
}

func newEnv(t *testing.T) *env { t.Helper(); return newEnvExe(t, "") }

func newEnvExe(t *testing.T, exe string) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	t.Setenv("NEXUSRUN_HOME", home)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	p := &project{root: root, shared: &shared{store: st, exe: exe, token: token, code: "0123456789abcdef0123456789abcdef", codeBy: time.Now().Add(time.Minute), projects: map[string]*project{}}}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = server.ProtectExcept(p.routes(), ts.Listener.Addr().String(), token, "/api/exchange")
	e := &env{t: t, root: root, srv: ts, p: p}
	ts.Start()
	t.Cleanup(ts.Close)
	return e
}

func (e *env) do(method, path string, body any, headers ...string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (e *env) readFile(rel string) string {
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestEveryAPICallNeedsTheTokenAndALoopbackHost(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, method, path string
		headers            []string
		want               int
	}{
		{"no token", "GET", "/api/project", []string{"Authorization", ""}, 401},
		{"wrong token", "GET", "/api/models", []string{"Authorization", "Bearer nope"}, 401},
		{"rebound host", "GET", "/api/project", []string{"Host", "evil.example:80"}, 403},
		{"cross-site write", "PUT", "/api/layout", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"foreign origin write", "PUT", "/api/layout", []string{"Origin", "http://evil.example"}, 403},
		{"ok", "GET", "/api/project", nil, 200},
	} {
		var body any
		if c.method == "PUT" {
			body = "{}"
		}
		req, _ := http.NewRequest(c.method, e.srv.URL+c.path, strings.NewReader(func() string {
			if body != nil {
				return "{}"
			}
			return ""
		}()))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(c.headers); i += 2 {
			switch {
			case c.headers[i] == "Host":
				req.Host = c.headers[i+1]
			case c.headers[i+1] == "":
				req.Header.Del(c.headers[i])
			default:
				req.Header.Set(c.headers[i], c.headers[i+1])
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if len(e.readFile(".nexusrun/studio.json")) != 0 {
		t.Error("a refused write must not reach the disk")
	}
}

func TestPageHasAScriptHashPolicy(t *testing.T) {
	csp := contentSecurityPolicy([]byte("<style>a{}</style><script>alert(1)</script>"))
	if !strings.Contains(csp, "script-src 'sha256-") || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("policy = %s", csp)
	}
}

func TestRenderValidatesAndSaveKeepsAOriginalBackup(t *testing.T) {
	e := newEnv(t)
	spec := map[string]any{"spec": map[string]any{
		"name": "demo", "version": "0.1.0",
		"agents":  map[string]any{"a": map[string]any{"unit": "a:1"}, "b": map[string]any{"unit": "b:1"}},
		"routing": []any{map[string]any{"from": "a", "to": "b", "condition": "a.nope > 1"}},
	}}
	code, out := e.do("POST", "/api/render", spec)
	var rep struct {
		YAML        string `json:"yaml"`
		Diagnostics []struct{ Path string }
	}
	json.Unmarshal(out, &rep)
	if code != 200 || !strings.Contains(rep.YAML, "kind: Workflow") || len(rep.Diagnostics) != 1 || rep.Diagnostics[0].Path != "routing[0].condition" {
		t.Fatalf("render = %d %s", code, out)
	}

	original := "# my notes\napiVersion: nexusrun.dev/v1\n"
	os.WriteFile(filepath.Join(e.root, "nexus-compose.yaml"), []byte(original), 0o644)
	code, out = e.do("GET", "/api/project", nil)
	if code != 200 || !strings.Contains(string(out), `"has_comments":true`) {
		t.Fatalf("project = %d %s", code, out)
	}
	if c, _ := e.do("PUT", "/api/workflow", map[string]string{"yaml": "first"}); c != 200 {
		t.Fatal("save failed")
	}
	e.do("PUT", "/api/workflow", map[string]string{"yaml": "second"})
	if e.readFile("nexus-compose.yaml") != "second" {
		t.Error("the file should hold the latest save")
	}
	if e.readFile("nexus-compose.yaml.bak") != original {
		t.Errorf("the backup must hold the original, got %q", e.readFile("nexus-compose.yaml.bak"))
	}
}

func TestUnitsAreCreatedEditedAndOnlyForSafeNames(t *testing.T) {
	e := newEnv(t)
	prompt, model := "You are terse.", "ollama:llama3.1:8b"
	code, out := e.do("PUT", "/api/unit?agent=fact_checker", map[string]any{"prompt": prompt, "model": model, "context": 4096})
	if code != 200 {
		t.Fatalf("create = %d %s", code, out)
	}
	file := e.readFile("agents/fact_checker/nexus.yaml")
	for _, want := range []string{"name: fact-checker", "source: ollama:llama3.1:8b", "type: chat", "You are terse."} {
		if !strings.Contains(file, want) {
			t.Errorf("unit file lacks %q:\n%s", want, file)
		}
	}
	// Editing the prompt leaves the model alone.
	e.do("PUT", "/api/unit?agent=fact_checker", map[string]any{"prompt": "Be kind."})
	_, out = e.do("GET", "/api/unit?agent=fact_checker", nil)
	var v unitView
	json.Unmarshal(out, &v)
	if v.Prompt != "Be kind." || v.Model != model || v.Context != 4096 || !v.Editable {
		t.Errorf("view = %+v", v)
	}
	// A draft without a model is valid and visibly unset.
	e.do("PUT", "/api/unit?agent=draft", map[string]any{"prompt": "hi"})
	_, out = e.do("GET", "/api/unit?agent=draft", nil)
	json.Unmarshal(out, &v)
	if v.Model != "" || !v.Exists {
		t.Errorf("draft view = %+v", v)
	}
	for _, bad := range []string{"../escape", "a/b", "", "9lives", "x y", ".hidden", strings.Repeat("a", 65)} {
		if c, _ := e.do("PUT", "/api/unit?agent="+bad, map[string]any{"prompt": "x"}); c != 400 {
			t.Errorf("agent %q: status %d, want 400", bad, c)
		}
	}
	if c, _ := e.do("PUT", "/api/unit?agent=fact_checker", map[string]any{"context": -5}); c != 400 {
		t.Errorf("negative context: status %d", c)
	}
}

func TestStudioWillNotRewriteUnitsItCannotRepresent(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "agents", "scripted")
	os.MkdirAll(dir, 0o755)
	script := "apiVersion: nexusrun.dev/v1\nname: scripted\nversion: 1.0.0\nentrypoint:\n  type: script\n  script: run.py\n"
	os.WriteFile(filepath.Join(dir, "nexus.yaml"), []byte(script), 0o644)
	if c, _ := e.do("PUT", "/api/unit?agent=scripted", map[string]any{"prompt": "x"}); c != 409 {
		t.Errorf("a script unit must be refused, got %d", c)
	}
	if e.readFile("agents/scripted/nexus.yaml") != script {
		t.Error("the refused unit was modified")
	}
	broken := filepath.Join(e.root, "agents", "broken")
	os.MkdirAll(broken, 0o755)
	os.WriteFile(filepath.Join(broken, "nexus.yaml"), []byte("not: [valid"), 0o644)
	if c, _ := e.do("PUT", "/api/unit?agent=broken", map[string]any{"prompt": "x"}); c != 409 {
		t.Errorf("a broken unit file must be refused, got %d", c)
	}
}

func TestWritesCannotFollowSymlinksOutOfTheProject(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.root, "agents")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if c, _ := e.do("PUT", "/api/unit?agent=evil", map[string]any{"prompt": "x", "model": "m"}); c != 400 {
		t.Errorf("status %d, want 400", c)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a write escaped the project: %v", entries)
	}
	os.Symlink(outside, filepath.Join(e.root, ".nexusrun"))
	e.do("PUT", "/api/layout", map[string]any{"a": 1})
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the layout write escaped the project: %v", entries)
	}
	os.Remove(filepath.Join(e.root, "nexus-compose.yaml"))
	target := filepath.Join(outside, "victim")
	os.WriteFile(target, []byte("keep"), 0o644)
	os.Symlink(target, filepath.Join(e.root, "nexus-compose.yaml"))
	if c, _ := e.do("PUT", "/api/workflow", map[string]string{"yaml": "x"}); c != 400 {
		t.Errorf("a symlinked workflow file must be refused, got %d", c)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Error("the symlink target was overwritten")
	}
}

func TestLayoutAndSettingsAreValidated(t *testing.T) {
	e := newEnv(t)
	if c, _ := e.do("PUT", "/api/layout", "not json"); c != 400 {
		t.Errorf("bad layout: %d", c)
	}
	if c, _ := e.do("PUT", "/api/layout", map[string]any{"a": map[string]int{"x": 1, "y": 2}}); c != 200 {
		t.Errorf("good layout: %d", c)
	}
	if !strings.Contains(e.readFile(".nexusrun/studio.json"), `"x":1`) {
		t.Error("layout not saved")
	}
	good := t.TempDir()
	for _, c := range []struct {
		dirs []string
		want int
	}{
		{[]string{"relative/path"}, 400},
		{[]string{"/definitely/not/here"}, 400},
		{[]string{filepath.Join(good, "nofolder")}, 400},
		{[]string{good}, 200},
	} {
		if code, out := e.do("PUT", "/api/settings", map[string]any{"model_dirs": c.dirs}); code != c.want {
			t.Errorf("dirs %v: %d %s, want %d", c.dirs, code, out, c.want)
		}
	}
	tooMany := make([]string, maxModelDirs+1)
	for i := range tooMany {
		tooMany[i] = good
	}
	if c, _ := e.do("PUT", "/api/settings", map[string]any{"model_dirs": tooMany}); c != 400 {
		t.Errorf("too many folders: %d", c)
	}
}

func TestModelsListsConfiguredFolders(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	if c, _ := e.do("PUT", "/api/settings", map[string]any{"model_dirs": []string{dir}}); c != 200 {
		t.Fatal("settings")
	}
	code, out := e.do("GET", "/api/models", nil)
	if code != 200 || !strings.Contains(string(out), `"hardware"`) {
		t.Fatalf("models = %d %s", code, out)
	}
}

func TestRenderUsesTwoSpaceIndentation(t *testing.T) {
	e := newEnv(t)
	_, out := e.do("POST", "/api/render", map[string]any{"spec": map[string]any{
		"name": "demo", "version": "0.1.0", "agents": map[string]any{"a": map[string]any{"unit": "a:1"}},
	}})
	var rep struct{ YAML string }
	json.Unmarshal(out, &rep)
	if !strings.Contains(rep.YAML, "\nagents:\n  a:\n    unit: a:1\n") {
		t.Errorf("want two-space indentation, got:\n%s", rep.YAML)
	}
}

func TestOneTimeCodeIsExchangedForTheTokenOnce(t *testing.T) {
	e := newEnv(t)
	post := func(code string, headers ...string) (int, string) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/exchange", strings.NewReader(`{"code":"`+code+`"}`))
		req.Header.Set("Content-Type", "application/json") // note: no Authorization header
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	good := e.p.code
	if c, _ := post("0000", nil...); c != 403 {
		t.Errorf("a wrong code: %d", c)
	}
	if c, _ := post(good, "Origin", "http://evil.example"); c != 403 {
		t.Errorf("a foreign page must not be able to redeem it: %d", c)
	}
	c, body := post(good)
	if c != 200 || !strings.Contains(body, token) {
		t.Fatalf("the code should yield the token once: %d %s", c, body)
	}
	if c, body := post(good); c != 403 || strings.Contains(body, token) {
		t.Errorf("a second use must fail without revealing anything: %d %s", c, body)
	}

	e2 := newEnv(t)
	e2.p.codeBy = time.Now().Add(-time.Second)
	req, _ := http.NewRequest("POST", e2.srv.URL+"/api/exchange", strings.NewReader(`{"code":"`+e2.p.code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("an expired code: %d", resp.StatusCode)
	}
	// Everything else still needs the token.
	if c, _ := e.do("GET", "/api/project", nil, "Authorization", ""); c != 401 {
		t.Errorf("the exchange must not open other routes: %d", c)
	}
}
