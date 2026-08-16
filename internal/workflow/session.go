package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/store"
)

// A session is a workflow started with `compose up -d` and still running.
//
// There is no daemon and no supervisor process: the record below is a
// small JSON file naming the detached process, and `compose down` reads it
// to find what to signal. That is the whole mechanism, and it is deliberate
// — a control plane that must itself be kept alive is exactly what
// NexusRun exists not to require.

// Session records a detached workflow run.
type Session struct {
	Name      string    `json:"name"`
	Ref       string    `json:"ref"`
	File      string    `json:"file"`
	PID       int       `json:"pid"`
	StatePath string    `json:"state_path,omitempty"`
	Started   time.Time `json:"started"`
}

func sessionsDir(s *store.Store) string { return filepath.Join(s.Root, "workflows") }

func sessionPath(s *store.Store, name string) string {
	return filepath.Join(sessionsDir(s), name+".json")
}

// SaveSession records a detached run.
func SaveSession(s *store.Store, sess *Session) error {
	if err := os.MkdirAll(sessionsDir(s), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sessionPath(s, sess.Name), data, 0o644)
}

// LoadSession reads one session by workflow name.
func LoadSession(s *store.Store, name string) (*Session, error) {
	data, err := os.ReadFile(sessionPath(s, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no detached run named %q — `%s compose ls` shows what is running", name, "nexus")
		}
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// RemoveSession forgets a session.
func RemoveSession(s *store.Store, name string) error {
	err := os.Remove(sessionPath(s, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Sessions lists recorded sessions, newest first.
//
// Records whose process has exited are pruned as they are read. A stale
// pidfile is the normal end state of a workflow that simply finished, and
// leaving it would make `compose ls` a list of things that are not running.
func Sessions(s *store.Store) ([]*Session, error) {
	entries, err := os.ReadDir(sessionsDir(s))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := LoadSession(s, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if !Alive(sess.PID) {
			_ = RemoveSession(s, sess.Name)
			continue
		}
		out = append(out, sess)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}
