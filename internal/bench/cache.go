package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// DefaultTTL is how long a benchmark result is trusted before it is
// measured again. Seven days is long enough that repeat runs are instant
// and short enough to notice a driver update, a rebuilt llama.cpp, or a
// machine that has started thermal-throttling.
const DefaultTTL = 7 * 24 * time.Hour

// Entry is one measured (unit, profile, model, target) result on this
// machine. Every field of the key matters: the same model scores
// differently on a different device, and against a different unit's suite.
type Entry struct {
	Unit    string `json:"unit"`
	Version string `json:"version"`
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Device  string `json:"device"`
	Backend string `json:"backend"`

	// Passed and Total are the eval result. EvalScore is their "12/13"
	// rendering, stored so the file is readable without doing arithmetic.
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	EvalScore string `json:"eval_score"`

	TokPerSec      float64 `json:"tok_per_sec"`
	LatencyMS      float64 `json:"latency_ms,omitempty"`
	ModelSizeBytes int64   `json:"model_size_bytes,omitempty"`
	ModelDigest    string  `json:"model_digest,omitempty"`

	// UnitDigest pins the result to the exact artifact evaluated. A unit
	// whose prompt changed is a different unit, and its old score is not
	// evidence about the new one.
	UnitDigest string `json:"unit_digest,omitempty"`

	Timestamp time.Time `json:"timestamp"`
}

// Rate is the pass rate as a percentage.
func (e Entry) Rate() float64 {
	if e.Total == 0 {
		return 0
	}
	return 100 * float64(e.Passed) / float64(e.Total)
}

// Fresh reports whether the entry is young enough to trust.
func (e Entry) Fresh(ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return time.Since(e.Timestamp) < ttl
}

// Cache is the on-disk benchmark record for one machine.
type Cache struct {
	MachineID string  `json:"machine_id"`
	Entries   []Entry `json:"entries"`
}

// CachePath is where benchmark results live.
//
// The roadmap wrote ~/.nexus/cache/benchmarks.json; the store actually
// roots at ~/.nexusrun (or $NEXUSRUN_HOME), and splitting state across two
// home directories would be a bug the first time someone set that variable.
func CachePath(s *store.Store) string {
	return filepath.Join(s.Root, "cache", "benchmarks.json")
}

// MachineID fingerprints the host so results measured on one machine are
// never applied to another.
//
// The inputs are the things that change measured throughput: the CPU, the
// accelerators, how much memory there is, and the OS/architecture. It is
// deliberately not a serial number or MAC address — this identifies a
// hardware profile, not a person, and it is safe to publish alongside a
// crowdsourced benchmark.
func MachineID(hw *hardware.Report) string {
	if hw == nil {
		hw = hardware.Detect()
	}
	var parts []string
	parts = append(parts,
		hw.OS, hw.Arch, hw.CPUModel,
		fmt.Sprint(hw.CPUCores),
		// RAM is rounded to the nearest GB: the exact free-page count
		// differs between boots and would invalidate the cache every time.
		fmt.Sprintf("%dGB", (hw.TotalRAMMB+512)/1024),
	)
	feats := append([]string(nil), hw.CPUFeatures...)
	sort.Strings(feats)
	parts = append(parts, strings.Join(feats, ","))

	var devs []string
	for _, d := range hw.Devices {
		devs = append(devs, d.Class+"/"+d.Vendor+"/"+d.Name)
	}
	sort.Strings(devs)
	parts = append(parts, devs...)

	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LoadCache reads this machine's benchmark cache.
//
// A cache belonging to another machine is discarded rather than merged:
// the file may have been copied along with a home directory, and a
// throughput number from a workstation applied to a Pi is worse than no
// number at all.
func LoadCache(s *store.Store) (*Cache, error) {
	id := MachineID(nil)
	data, err := os.ReadFile(CachePath(s))
	if err != nil {
		if os.IsNotExist(err) {
			return &Cache{MachineID: id}, nil
		}
		return nil, err
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		// A corrupt cache is a performance problem, not a correctness one.
		return &Cache{MachineID: id}, nil
	}
	if c.MachineID != id {
		return &Cache{MachineID: id}, nil
	}
	return &c, nil
}

// SaveCache writes the cache atomically.
func SaveCache(s *store.Store, c *Cache) error {
	if c.MachineID == "" {
		c.MachineID = MachineID(nil)
	}
	if err := os.MkdirAll(filepath.Dir(CachePath(s)), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := CachePath(s) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, CachePath(s))
}

// ClearCache removes the cache file.
func ClearCache(s *store.Store) error {
	err := os.Remove(CachePath(s))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Key identifies an entry for lookup and replacement.
type Key struct {
	Unit, Version, Profile, Model string
}

func (e Entry) key() Key {
	return Key{Unit: e.Unit, Version: e.Version, Profile: e.Profile, Model: e.Model}
}

// Lookup returns the freshest matching entry, or nil.
//
// unitDigest, when given, must match: a rebuilt unit with a changed prompt
// has no business inheriting the old unit's scores.
func (c *Cache) Lookup(k Key, unitDigest string, ttl time.Duration) *Entry {
	var best *Entry
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.key() != k {
			continue
		}
		if unitDigest != "" && e.UnitDigest != "" && e.UnitDigest != unitDigest {
			continue
		}
		if !e.Fresh(ttl) {
			continue
		}
		if best == nil || e.Timestamp.After(best.Timestamp) {
			best = e
		}
	}
	return best
}

// Put inserts an entry, replacing any earlier result for the same key.
func (c *Cache) Put(e Entry) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.EvalScore == "" && e.Total > 0 {
		e.EvalScore = fmt.Sprintf("%d/%d", e.Passed, e.Total)
	}
	for i := range c.Entries {
		if c.Entries[i].key() == e.key() {
			c.Entries[i] = e
			return
		}
	}
	c.Entries = append(c.Entries, e)
}

// Sorted returns entries newest first, for display.
func (c *Cache) Sorted() []Entry {
	out := append([]Entry(nil), c.Entries...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}

// String renders the cache as a readable table.
func (c *Cache) String() string {
	if len(c.Entries) == 0 {
		return "No benchmark results cached for this machine.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Machine: %s\n\n", shortID(c.MachineID))

	// Widths follow the data. "llama.cpp/server/cpu" overflowed a fixed
	// target column and pushed every later field on its row out of line.
	unitW, modelW, profW, targetW := len("UNIT"), len("MODEL"), len("PROFILE"), len("TARGET")
	for _, e := range c.Entries {
		unitW = max(unitW, len(e.Unit+":"+e.Version))
		modelW = max(modelW, len(e.Model))
		profW = max(profW, len(e.Profile))
		targetW = max(targetW, len(e.Backend+"/"+e.Device))
	}
	row := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%-%ds  %%7s  %%9s  %%s\n", unitW, profW, modelW, targetW)
	fmt.Fprintf(&b, row, "UNIT", "PROFILE", "MODEL", "TARGET", "SCORE", "TOK/S", "AGE")
	for _, e := range c.Sorted() {
		score := e.EvalScore
		if score == "" {
			score = "—"
		}
		fmt.Fprintf(&b, row,
			e.Unit+":"+e.Version, e.Profile, e.Model,
			e.Backend+"/"+e.Device, score,
			fmt.Sprintf("%.1f", e.TokPerSec), humanAge(time.Since(e.Timestamp)))
	}
	return b.String()
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
