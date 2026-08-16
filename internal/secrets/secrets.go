// Package secrets keeps an agent's credentials out of its unit file.
//
// A unit is meant to be committed, pushed to a public registry, and read by
// whoever is about to run it. An API key in that file is a key in git
// history and in every registry that ever mirrored the artifact. So the
// unit declares only that it *needs* a secret, and the value lives here —
// encrypted, on the machine that runs the agent, scoped to that agent and
// optionally to that one device.
//
// The store is a single encrypted file rather than SQLite, for the reason
// given in package workflow: a pure-Go SQLite driver is larger than this
// entire binary, and the cgo ones break the static cross-compilation to
// 32-bit ARM that NexusRun exists to support. The data model below is the
// roadmap's schema; only the storage engine differs.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/verdictlayer/nexusrun/internal/store"
)

const (
	// MasterKeyEnv overrides the on-disk master key.
	MasterKeyEnv = "NEXUS_MASTER_KEY"

	// DefaultGrace is how long a rotated secret's previous value stays
	// accepted. A long-running agent holding the old value must not start
	// failing the instant a new one is written.
	DefaultGrace = 5 * time.Minute
)

// Secret is one stored credential. Value and Previous are ciphertext; the
// plaintext exists only inside Reveal and is never written anywhere.
type Secret struct {
	Agent string `json:"agent"`

	// Device scopes a secret to one machine. Empty means it applies
	// everywhere, and a device-scoped secret always wins over a global one
	// with the same key — that is how one fleet ships per-site credentials
	// without a per-site unit.
	Device string `json:"device,omitempty"`

	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int    `json:"version"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Previous is the value this one replaced, kept until RotatedAt plus
	// the grace period so a rotation is not a hard cutover.
	Previous  string     `json:"previous,omitempty"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
}

// Expired reports whether the secret is past its expiry.
func (s Secret) Expired() bool {
	return s.ExpiresAt != nil && time.Now().After(*s.ExpiresAt)
}

// Scope renders the secret's scope for display.
func (s Secret) Scope() string {
	if s.Device == "" {
		return "all devices"
	}
	return "device " + s.Device
}

// Store is the encrypted secret store.
type Store struct {
	mu      sync.Mutex
	root    *store.Store
	key     []byte
	secrets []Secret
}

// Open loads the store, creating a master key on first use.
func Open(s *store.Store) (*Store, error) {
	key, err := masterKey(s)
	if err != nil {
		return nil, err
	}
	st := &Store{root: s, key: key}
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

// Path is the store file's location.
func Path(s *store.Store) string { return filepath.Join(s.Root, "secrets.json") }

// KeyPath is the master key's location.
func KeyPath(s *store.Store) string { return filepath.Join(s.Root, "master.key") }

// AuditPath is the access log's location.
func AuditPath(s *store.Store) string { return filepath.Join(s.Root, "audit.log") }

// masterKey returns the 32-byte key, generating one if none exists.
//
// The environment wins over the file so a fleet can inject a key it never
// writes to disk. Generating on first use rather than demanding setup is
// deliberate: a store nobody can write to is a store people work around by
// putting the key back in the YAML.
func masterKey(s *store.Store) ([]byte, error) {
	if raw := os.Getenv(MasterKeyEnv); strings.TrimSpace(raw) != "" {
		sum := sha256.Sum256([]byte(raw))
		return sum[:], nil
	}
	path := KeyPath(s)
	data, err := os.ReadFile(path)
	if err == nil {
		key, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a valid master key; move it aside to generate a new one (every stored secret becomes unreadable)", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (st *Store) load() error {
	data, err := os.ReadFile(Path(st.root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &st.secrets)
}

func (st *Store) save() error {
	data, err := json.MarshalIndent(st.secrets, "", "  ")
	if err != nil {
		return err
	}
	path := Path(st.root)
	tmp := path + ".tmp"
	// 0600 even though the values are encrypted: the key names alone say
	// which services an agent talks to.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- crypto ---------------------------------------------------------------

func (st *Store) seal(plain string) (string, error) {
	gcm, err := newGCM(st.key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (st *Store) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("stored value is not valid base64")
	}
	gcm, err := newGCM(st.key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("stored value is too short to be encrypted")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt failed — the master key does not match the one these secrets were written with")
	}
	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// --- operations -----------------------------------------------------------

// SetOptions configures a write.
type SetOptions struct {
	Device  string
	Expires *time.Time
}

func (st *Store) index(agent, device, key string) int {
	for i, s := range st.secrets {
		if s.Agent == agent && s.Device == device && s.Key == key {
			return i
		}
	}
	return -1
}

// Set stores or replaces a secret.
func (st *Store) Set(agent, key, value string, opts SetOptions) error {
	if err := validName(agent, "agent"); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	sealed, err := st.seal(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if i := st.index(agent, opts.Device, key); i >= 0 {
		s := &st.secrets[i]
		s.Value, s.UpdatedAt, s.ExpiresAt = sealed, now, opts.Expires
		s.Version++
		// A plain overwrite is not a rotation: there is no grace period and
		// the old value is gone immediately, which is what someone fixing a
		// typo expects.
		s.Previous, s.RotatedAt = "", nil
	} else {
		st.secrets = append(st.secrets, Secret{
			Agent: agent, Device: opts.Device, Key: key, Value: sealed,
			Version: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: opts.Expires,
		})
	}
	if err := st.save(); err != nil {
		return err
	}
	st.audit("set", agent, opts.Device, key)
	return nil
}

// Rotate replaces a value while keeping the old one valid for a grace
// period, so an agent already holding it does not fail mid-request.
func (st *Store) Rotate(agent, key, value, device string) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	i := st.index(agent, device, key)
	if i < 0 {
		return fmt.Errorf("no secret %s for agent %s", key, agent)
	}
	sealed, err := st.seal(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	s := &st.secrets[i]
	s.Previous, s.Value = s.Value, sealed
	s.RotatedAt, s.UpdatedAt = &now, now
	s.Version++
	if err := st.save(); err != nil {
		return err
	}
	st.audit("rotate", agent, device, key)
	return nil
}

// Remove deletes a secret. It reports whether anything was removed.
func (st *Store) Remove(agent, key, device string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	i := st.index(agent, device, key)
	if i < 0 {
		return false, nil
	}
	st.secrets = append(st.secrets[:i], st.secrets[i+1:]...)
	if err := st.save(); err != nil {
		return false, err
	}
	st.audit("remove", agent, device, key)
	return true, nil
}

// List returns an agent's secrets, or every secret when agent is empty.
// Values are never included.
func (st *Store) List(agent string) []Secret {
	st.mu.Lock()
	defer st.mu.Unlock()

	var out []Secret
	for _, s := range st.secrets {
		if agent != "" && s.Agent != agent {
			continue
		}
		s.Value, s.Previous = "", "" // never leaves the package
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Device < out[j].Device
	})
	return out
}

// Reveal returns one secret's plaintext. It is the only way a value leaves
// the store, and every call is logged.
func (st *Store) Reveal(agent, key, device string) (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	s, ok := st.resolve(agent, key, device)
	if !ok {
		return "", fmt.Errorf("no secret %s for agent %s", key, agent)
	}
	if s.Expired() {
		return "", fmt.Errorf("secret %s for agent %s expired at %s",
			key, agent, s.ExpiresAt.Format(time.RFC3339))
	}
	v, err := st.open(s.Value)
	if err != nil {
		return "", err
	}
	st.audit("read", agent, s.Device, key)
	return v, nil
}

// resolve finds the secret that applies, preferring a device-scoped entry
// over a global one. Callers hold the lock.
func (st *Store) resolve(agent, key, device string) (Secret, bool) {
	var global Secret
	haveGlobal := false
	for _, s := range st.secrets {
		if s.Agent != agent || s.Key != key {
			continue
		}
		if s.Device != "" && s.Device == device {
			return s, true
		}
		if s.Device == "" {
			global, haveGlobal = s, true
		}
	}
	return global, haveGlobal
}

// Resolved is one secret prepared for injection.
type Resolved struct {
	Key   string
	Value string

	// Previous is the pre-rotation value, still inside its grace period.
	// It is offered as KEY_PREVIOUS so an agent can accept either during a
	// rotation instead of failing on whichever half it did not get.
	Previous string
}

// Env resolves the named secrets for an agent into injectable values.
//
// Missing keys are returned separately rather than as an error, because
// which ones are *required* is the manifest's business, not the store's.
func (st *Store) Env(agent, device string, keys []string, grace time.Duration) (map[string]Resolved, []string, error) {
	if grace <= 0 {
		grace = DefaultGrace
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	out := map[string]Resolved{}
	var missing []string
	for _, k := range keys {
		s, ok := st.resolve(agent, k, device)
		if !ok || s.Expired() {
			missing = append(missing, k)
			continue
		}
		v, err := st.open(s.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", k, err)
		}
		r := Resolved{Key: k, Value: v}
		if s.Previous != "" && s.RotatedAt != nil && time.Since(*s.RotatedAt) < grace {
			if prev, perr := st.open(s.Previous); perr == nil {
				r.Previous = prev
			}
		}
		out[k] = r
		st.audit("inject", agent, s.Device, k)
	}
	return out, missing, nil
}

// --- backup ---------------------------------------------------------------

// Export writes an encrypted backup. The payload is re-encrypted under a
// passphrase rather than the master key, so a backup is portable to a
// machine that does not have this machine's key.
func (st *Store) Export(agent, passphrase string) ([]byte, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	type entry struct {
		Secret
		Plain string `json:"plain"`
	}
	var entries []entry
	for _, s := range st.secrets {
		if agent != "" && s.Agent != agent {
			continue
		}
		plain, err := st.open(s.Value)
		if err != nil {
			return nil, err
		}
		e := entry{Secret: s, Plain: plain}
		e.Value, e.Previous = "", "" // the backup carries plaintext, once, sealed below
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no secrets to export")
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256([]byte(passphrase))
	gcm, err := newGCM(sum[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, body, nil)

	st.audit("export", agent, "", fmt.Sprintf("%d secret(s)", len(entries)))
	// Framed so an import can tell a truncated file from a wrong passphrase.
	out := append([]byte("nexusrun-secrets-v1\n"), []byte(base64.StdEncoding.EncodeToString(sealed))...)
	return append(out, '\n'), nil
}

// Import restores a backup, re-encrypting under this machine's master key.
func (st *Store) Import(data []byte, passphrase string) (int, error) {
	text := strings.TrimSpace(string(data))
	header, body, ok := strings.Cut(text, "\n")
	if !ok || strings.TrimSpace(header) != "nexusrun-secrets-v1" {
		return 0, fmt.Errorf("not a NexusRun secrets backup")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
	if err != nil {
		return 0, fmt.Errorf("backup is corrupt: %w", err)
	}
	sum := sha256.Sum256([]byte(passphrase))
	gcm, err := newGCM(sum[:])
	if err != nil {
		return 0, err
	}
	if len(raw) < gcm.NonceSize() {
		return 0, fmt.Errorf("backup is truncated")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return 0, fmt.Errorf("decrypt failed — wrong passphrase for this backup")
	}

	type entry struct {
		Secret
		Plain string `json:"plain"`
	}
	var entries []entry
	if err := json.Unmarshal(plain, &entries); err != nil {
		return 0, err
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range entries {
		sealed, serr := st.seal(e.Plain)
		if serr != nil {
			return 0, serr
		}
		s := e.Secret
		s.Value, s.Previous, s.RotatedAt = sealed, "", nil
		if i := st.index(s.Agent, s.Device, s.Key); i >= 0 {
			st.secrets[i] = s
		} else {
			st.secrets = append(st.secrets, s)
		}
	}
	if err := st.save(); err != nil {
		return 0, err
	}
	st.audit("import", "", "", fmt.Sprintf("%d secret(s)", len(entries)))
	return len(entries), nil
}

// --- audit ----------------------------------------------------------------

// audit appends one line per operation. Key *names* are logged; values
// never are. Callers hold the lock. A failure to log is not allowed to
// fail the operation — losing an audit line is bad, refusing to start an
// agent because the disk is full is worse.
func (st *Store) audit(op, agent, device, key string) {
	f, err := os.OpenFile(AuditPath(st.root), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	scope := device
	if scope == "" {
		scope = "-"
	}
	if agent == "" {
		agent = "-"
	}
	fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\n",
		time.Now().UTC().Format(time.RFC3339), op, agent, scope, key)
}

// AuditEntry is one parsed audit line.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Op     string    `json:"op"`
	Agent  string    `json:"agent"`
	Device string    `json:"device,omitempty"`
	Key    string    `json:"key"`
}

// Audit reads the log, newest first, optionally filtered by agent.
func Audit(s *store.Store, agent string, limit int) ([]AuditEntry, error) {
	data, err := os.ReadFile(AuditPath(s))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []AuditEntry
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			continue
		}
		ts, terr := time.Parse(time.RFC3339, parts[0])
		if terr != nil {
			continue
		}
		e := AuditEntry{Time: ts, Op: parts[1], Agent: parts[2], Device: parts[3], Key: parts[4]}
		if e.Device == "-" {
			e.Device = ""
		}
		if agent != "" && e.Agent != agent {
			continue
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- validation -----------------------------------------------------------

func validName(v, what string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", what)
	}
	return nil
}

// validKey enforces the environment-variable name shape. A secret is
// injected as an env var, and a key that cannot be one would be stored
// happily and then silently never reach the agent.
func validKey(k string) error {
	if k == "" {
		return fmt.Errorf("secret key is required")
	}
	for i, r := range k {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf(
				"secret key %q must be a valid environment variable name (letters, digits, underscore; not starting with a digit) — it is injected as one",
				k)
		}
	}
	return nil
}
