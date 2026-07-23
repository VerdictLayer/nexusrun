// Package daemon keeps models resident in memory between runs.
//
// The CLI reloads a model's weights on every invocation. That is fine for
// a 1.5B model (about a second) and painful for a 30B one. A daemon holds
// llama-server processes open, so the first run pays the load cost and
// every later run against the same model skips it entirely.
//
// The daemon is optional by design: if it is not running, `nexus run`
// falls back to direct execution and behaves exactly as before.
package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/lanceseidman/nexusrun/internal/engine"
	"github.com/lanceseidman/nexusrun/internal/hardware"
	"github.com/lanceseidman/nexusrun/internal/manifest"
	"github.com/lanceseidman/nexusrun/internal/store"
	"github.com/lanceseidman/nexusrun/internal/unit"
)

// DefaultIdleTimeout is how long an unused model stays resident before
// its memory is released.
const DefaultIdleTimeout = 10 * time.Minute

// Pool holds warm models and executes unit runs against them.
type Pool struct {
	store  *store.Store
	server *engine.LlamaServer

	mu   sync.Mutex
	hits int
	miss int
}

// NewPool creates a warm-model pool backed by llama-server.
func NewPool(s *store.Store) *Pool {
	return &Pool{store: s, server: &engine.LlamaServer{}}
}

// Available reports whether warm execution is possible on this host.
func (p *Pool) Available() bool {
	return p.server.Probe().Available
}

// RunRequest describes a unit execution.
type RunRequest struct {
	Unit        string   `json:"unit"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int      `json:"max_tokens"`
	Device      string   `json:"device"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// RunResponse is the result of one execution.
type RunResponse struct {
	Text      string  `json:"text"`
	Unit      string  `json:"unit"`
	Backend   string  `json:"backend"`
	Device    string  `json:"device"`
	TokensOut int     `json:"tokens_out"`
	EvalTPS   float64 `json:"eval_tokens_per_sec"`
	PromptTPS float64 `json:"prompt_tokens_per_sec"`
	Warm      bool    `json:"warm"`
	RunID     string  `json:"run_id"`
}

// Run executes a unit against a warm model, loading it if necessary.
func (p *Pool) Run(ctx context.Context, req RunRequest) (*RunResponse, error) {
	m, err := p.resolveManifest(ctx, req.Unit)
	if err != nil {
		return nil, err
	}
	if len(m.Models) == 0 {
		return nil, fmt.Errorf("unit %s declares no models", m.Ref())
	}
	mod := m.Models[0]

	resolved, err := p.store.Resolve(mod.Source, mod.SHA256, nil)
	if err != nil {
		return nil, err
	}

	hw := hardware.Detect()
	prefer := m.Hardware.Prefer
	if req.Device != "" {
		prefer = []string{req.Device}
	}
	device := hardware.ClassCPU
	cap := p.server.Probe()
	for _, want := range prefer {
		if !hw.Has(want) {
			continue
		}
		for _, have := range cap.Devices {
			if have == want {
				device = want
			}
		}
		if device == want {
			break
		}
	}

	// A model already resident is a hit; anything else pays a load.
	wasWarm := p.server.IsWarm(resolved.Path, device)
	p.mu.Lock()
	if wasWarm {
		p.hits++
	} else {
		p.miss++
	}
	p.mu.Unlock()

	temp := 0.7
	if mod.Temperature != nil {
		temp = *mod.Temperature
	}
	if req.Temperature != nil {
		temp = *req.Temperature
	}

	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + sanitize(m.Name)
	rec := &store.RunRecord{
		ID: runID, Unit: m.Ref(), Started: time.Now(),
		Device: device, Backend: p.server.Name(),
	}

	res, err := p.server.Generate(ctx, engine.Request{
		ModelPath:   resolved.Path,
		ModelRef:    mod.Source,
		Prompt:      req.Prompt,
		System:      m.Entrypoint.SystemPrompt,
		MaxTokens:   req.MaxTokens,
		Temperature: temp,
		Context:     mod.Context,
		Device:      device,
		Chat:        m.Entrypoint.Type == "chat",
	})
	rec.Ended = time.Now()
	if err != nil {
		rec.Error = err.Error()
		rec.ExitCode = 1
		_ = p.store.SaveRun(rec)
		return nil, err
	}
	rec.TokensOut = res.TokensOut
	rec.TokPerSec = res.EvalTPS
	_ = p.store.SaveRun(rec)
	_ = os.WriteFile(p.store.LogPath(runID), []byte(res.Text), 0o644)

	return &RunResponse{
		Text:      res.Text,
		Unit:      m.Ref(),
		Backend:   res.Backend,
		Device:    res.Device,
		TokensOut: res.TokensOut,
		EvalTPS:   res.EvalTPS,
		PromptTPS: res.PromptTPS,
		Warm:      wasWarm,
		RunID:     runID,
	}, nil
}

func (p *Pool) resolveManifest(ctx context.Context, ref string) (*manifest.Manifest, error) {
	if info, err := os.Stat(ref); err == nil && info.IsDir() {
		return manifest.Load(ref)
	}
	m, _, err := unit.Resolve(ctx, p.store, ref)
	return m, err
}

// Status describes what the pool is holding.
type Status struct {
	Warm      []string `json:"warm_models"`
	Hits      int      `json:"warm_hits"`
	Misses    int      `json:"cold_loads"`
	Available bool     `json:"available"`
}

// Status reports currently resident models and cache effectiveness.
func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{
		Warm:      p.server.Warm(),
		Hits:      p.hits,
		Misses:    p.miss,
		Available: p.server.Probe().Available,
	}
}

// StartEviction releases models idle for longer than the timeout, until
// the context is cancelled.
func (p *Pool) StartEviction(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				p.server.Shutdown()
				return
			case <-ticker.C:
				p.server.EvictIdle(idle)
			}
		}
	}()
}

// Shutdown stops every warm model process.
func (p *Pool) Shutdown() { p.server.Shutdown() }

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		}
	}
	return string(out)
}
