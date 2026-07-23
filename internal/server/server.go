// Package server provides the NexusRun web console: a local dashboard
// for inspecting units, hardware capability, and run history. It is
// read-only by design — management actions stay in the CLI until there
// is an auth story worth trusting.
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/daemon"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

//go:embed console.html
var consoleHTML []byte

// Serve starts the console on addr and blocks until the context ends.
//
// When pool is non-nil the server also acts as an inference daemon,
// holding models in memory so repeat runs skip the load cost.
func Serve(ctx context.Context, s *store.Store, addr string, pool *daemon.Pool) error {
	mux := http.NewServeMux()

	if pool != nil {
		// Executing a unit is the one mutating action exposed over HTTP.
		// It is bound to loopback by default; do not expose it publicly
		// without putting authentication in front of it.
		mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
			var req daemon.RunRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
				return
			}
			if req.Unit == "" {
				http.Error(w, "unit is required", http.StatusBadRequest)
				return
			}
			res, err := pool.Run(r.Context(), req)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, res)
		})

		mux.HandleFunc("GET /api/pool", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, pool.Status())
		})
	}

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(consoleHTML)
	})

	mux.HandleFunc("GET /api/hardware", func(w http.ResponseWriter, r *http.Request) {
		hw := hardware.Detect()
		caps := engine.ProbeAll()
		usable := map[string]bool{}
		for _, c := range caps {
			if c.Available {
				for _, d := range c.Devices {
					if hw.Has(d) {
						usable[d] = true
					}
				}
			}
		}
		var usableList []string
		for _, class := range []string{hardware.ClassNPU, hardware.ClassGPU, hardware.ClassCPU} {
			if usable[class] {
				usableList = append(usableList, class)
			}
		}
		writeJSON(w, map[string]any{
			"hardware": hw,
			"backends": caps,
			"usable":   usableList,
		})
	})

	mux.HandleFunc("GET /api/units", func(w http.ResponseWriter, r *http.Request) {
		refs, err := unit.List(r.Context(), s)
		if err != nil {
			writeErr(w, err)
			return
		}
		type unitInfo struct {
			Ref         string `json:"ref"`
			Description string `json:"description"`
			Size        int64  `json:"size"`
			Sealed      bool   `json:"sealed"`
			Models      int    `json:"models"`
			Prefer      string `json:"prefer"`
		}
		out := []unitInfo{}
		for _, ref := range refs {
			m, om, err := unit.Resolve(r.Context(), s, ref)
			if err != nil {
				continue
			}
			var size int64
			for _, l := range om.Layers {
				size += l.Size
			}
			out = append(out, unitInfo{
				Ref:         ref,
				Description: m.Description,
				Size:        size,
				Sealed:      om.Annotations[unit.AnnotationSealed] == "true",
				Models:      len(m.Models),
				Prefer:      strings.Join(m.Hardware.Prefer, " → "),
			})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		runs, err := s.Runs()
		if err != nil {
			writeErr(w, err)
			return
		}
		if runs == nil {
			runs = []*store.RunRecord{}
		}
		writeJSON(w, runs)
	})

	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Run IDs index into the logs directory, so reject anything that
		// could escape it.
		if id != filepath.Base(id) || strings.Contains(id, "..") {
			http.Error(w, "invalid run id", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(s.LogPath(id))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
