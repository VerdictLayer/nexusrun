package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/store"
)

// Save writes a report to the store so later runs can be compared against
// it. Reports are the durable artifact of this package: a score is only
// useful next to the score it replaced.
func Save(s *store.Store, r *Report) error {
	if r.ID == "" {
		return fmt.Errorf("report has no ID")
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.EvalsDir(), r.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadRecord reads a saved report by ID.
func LoadRecord(s *store.Store, id string) (*Report, error) {
	id = strings.TrimSuffix(id, ".json")
	data, err := os.ReadFile(filepath.Join(s.EvalsDir(), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("no saved evaluation %q: %w", id, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse saved evaluation %q: %w", id, err)
	}
	return &r, nil
}

// List returns saved reports, newest first.
func List(s *store.Store) ([]*Report, error) {
	entries, err := os.ReadDir(s.EvalsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Report
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.EvalsDir(), e.Name()))
		if err != nil {
			continue
		}
		var r Report
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		out = append(out, &r)
	}
	// IDs lead with a UTC timestamp, so lexical order is chronological.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Latest returns the most recent saved report for a unit, or nil. It is
// what makes `--compare` usable without copying IDs around: the common
// question is "did my change make this worse than last time".
func Latest(s *store.Store, unitRef string) (*Report, error) {
	all, err := List(s)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if unitRef == "" || r.Unit == unitRef {
			return r, nil
		}
	}
	return nil, nil
}
