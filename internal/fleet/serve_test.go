package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/agent"
	"github.com/verdictlayer/nexusrun/internal/engine"
)

type fakeChat struct {
	release string
	got     []engine.Message
	closed  bool
}

func (f *fakeChat) Chat(_ context.Context, msgs []engine.Message, _ int, stream func(string)) (*agent.Result, error) {
	f.got = msgs
	if stream != nil {
		stream("hel")
		stream("lo")
	}
	return &agent.Result{Text: "hello from " + f.release, TokensOut: 3}, nil
}
func (f *fakeChat) Close() { f.closed = true }

type harness struct {
	ep     *Endpoint
	active *Release
	opened []*fakeChat
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	_, r := fixture(t)
	h := &harness{active: &r}
	h.ep = &Endpoint{
		active: func() (*Release, error) {
			if h.active == nil {
				return nil, errNoActive
			}
			r := *h.active
			return &r, nil
		},
		open: func(_ context.Context, r Release) (chatter, string, error) {
			f := &fakeChat{release: r.ID}
			h.opened = append(h.opened, f)
			return f, "agent:" + r.ID[:4], nil
		},
	}
	return h
}

func (h *harness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ep.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChatAnswersWithTheActiveReleaseAndNamesIt(t *testing.T) {
	h := newHarness(t)
	rec := h.post(t, `{"model":"anything","messages":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "hello from "+h.active.ID || out.Model != "agent:2222" {
		t.Fatalf("%s", rec.Body)
	}
	if rec.Header().Get("X-Nexus-Release") != h.active.ID || rec.Header().Get("X-Nexus-Digest") != h.active.Digest {
		t.Fatal("response does not name the release that answered")
	}
	got := h.opened[0].got
	if len(got) != 2 || got[0].Role != "system" || got[1].Content != "hi" {
		t.Fatalf("messages %+v", got)
	}
}

func TestNewActiveReleaseIsLoadedAndTheOldOneClosed(t *testing.T) {
	h := newHarness(t)
	h.post(t, `{"messages":[{"role":"user","content":"a"}]}`)
	h.post(t, `{"messages":[{"role":"user","content":"b"}]}`)
	if len(h.opened) != 1 {
		t.Fatalf("reopened an unchanged release (%d opens)", len(h.opened))
	}
	next := *h.active
	next.ID = "33333333-3333-3333-3333-333333333333"
	h.active = &next
	rec := h.post(t, `{"messages":[{"role":"user","content":"c"}]}`)
	if len(h.opened) != 2 || !h.opened[0].closed || !strings.Contains(rec.Body.String(), next.ID) {
		t.Fatalf("did not switch releases: %s", rec.Body)
	}
}

func TestStreamingSendsChunksThenDone(t *testing.T) {
	h := newHarness(t)
	rec := h.post(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	var last string
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		last = line
		if line == "[DONE]" {
			break
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		text.WriteString(c.Choices[0].Delta.Content)
	}
	if text.String() != "hello" || last != "[DONE]" {
		t.Fatalf("streamed %q, last %q", text.String(), last)
	}
}

func TestChatRefusesWhatTheReleaseDoesNotDefine(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"client tools": `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"rm"}}]}`,
		"tool role":    `{"messages":[{"role":"tool","content":"x"}]}`,
		"no user":      `{"messages":[{"role":"system","content":"x"}]}`,
		"image":        `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		"not json":     `messages`,
	} {
		if rec := h.post(t, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if len(h.opened) != 0 {
		t.Fatal("opened the release for an invalid request")
	}
}

func TestNoActiveReleaseAndTestingAreUnavailable(t *testing.T) {
	h := newHarness(t)
	h.active = nil
	if rec := h.post(t, `{"messages":[{"role":"user","content":"x"}]}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no active release: %d", rec.Code)
	}
	h = newHarness(t)
	h.post(t, `{"messages":[{"role":"user","content":"x"}]}`)
	inPrepare := make(chan struct{})
	release := make(chan struct{})
	go h.ep.Prepare(func(context.Context, Enrollment, Release) Outcome {
		close(inPrepare)
		<-release
		return Outcome{}
	})(context.Background(), Enrollment{}, Release{})
	<-inPrepare
	if !h.opened[0].closed {
		t.Fatal("loaded release stayed in memory while a candidate was tested")
	}
	rec := h.post(t, `{"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("request during a test: %d", rec.Code)
	}
	close(release)
}

func TestAPIKeyIsPrivateAndStable(t *testing.T) {
	home, _ := fixture(t)
	a, err := APIKey(home)
	if err != nil {
		t.Fatal(err)
	}
	b, err := APIKey(home)
	if err != nil || a != b || !strings.HasPrefix(a, "nxf_") {
		t.Fatalf("%q %q %v", a, b, err)
	}
	if err = writePrivate(apiKeyPath(home), apiKey{"short"}); err != nil {
		t.Fatal(err)
	}
	if _, err = APIKey(home); err == nil {
		t.Fatal("accepted a malformed key file")
	}
}
