package workflow

import (
	"bufio"
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
	"strings"
	"sync"
	"time"
)

// StateKeyEnv names the environment variable holding the state key.
const StateKeyEnv = "NEXUS_STATE_KEY"

// Message is one delivery between agents. It is the workflow's audit
// record: every hop, what condition let it through, and whether the
// payload was reshaped on the way.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Timestamp time.Time `json:"timestamp"`

	Payload        Payload        `json:"payload"`
	RoutingContext RoutingContext `json:"routing_context"`
}

// Payload is what actually crossed, plus how it was produced.
type Payload struct {
	Content  string   `json:"content"`
	Metadata Metadata `json:"metadata"`
}

// Metadata records the conditions the content was generated under. It
// travels with the message because "the writer produced nonsense" and
// "the writer fell back to CPU and a 3B model" are the same incident, and
// only the second one is actionable.
type Metadata struct {
	TokensUsed int     `json:"tokens_used"`
	TokPerSec  float64 `json:"tokens_per_sec,omitempty"`
	Runtime    string  `json:"runtime,omitempty"`
	Device     string  `json:"device,omitempty"`
}

// RoutingContext says why this message exists.
type RoutingContext struct {
	Condition        string `json:"condition,omitempty"`
	ConditionMatched bool   `json:"condition_matched"`
	TransformApplied bool   `json:"transform_applied"`
}

// Bus is the shared state backend.
type Bus interface {
	Publish(Message) error
	Messages() ([]Message, error)
	Close() error
}

// OpenBus creates the bus a workflow declared. baseDir resolves a relative
// state path, so a workflow run from anywhere writes beside its own file
// rather than beside the shell's working directory.
func OpenBus(st State, baseDir string) (Bus, error) {
	switch st.Backend {
	case "", StateMemory:
		return &memoryBus{}, nil
	case StateFile:
		path := st.Path
		if path == "" {
			path = "./.nexus/state.jsonl"
		}
		if !filepath.IsAbs(path) && baseDir != "" {
			path = filepath.Join(baseDir, path)
		}
		return openFileBus(path, st.Encryption)
	default:
		return nil, fmt.Errorf("unsupported shared_state.backend %q", st.Backend)
	}
}

// --- memory ---------------------------------------------------------------

type memoryBus struct {
	mu   sync.Mutex
	msgs []Message
}

func (b *memoryBus) Publish(m Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return nil
}

func (b *memoryBus) Messages() ([]Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.msgs...), nil
}

func (b *memoryBus) Close() error { return nil }

// --- file -----------------------------------------------------------------

// fileBus is an append-only JSONL log: one message per line, durable
// across a crash, readable with any text tool when unencrypted, and with
// no index to corrupt. Appends are O(1) and the file is the audit trail.
type fileBus struct {
	mu   sync.Mutex
	path string
	f    *os.File
	key  []byte // nil when unencrypted
}

func openFileBus(path, encryption string) (*fileBus, error) {
	var key []byte
	if encryption == EncryptionAESGCM {
		k, err := stateKey()
		if err != nil {
			return nil, err
		}
		key = k
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 0600: the bus holds whatever the agents said to each other.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileBus{path: path, f: f, key: key}, nil
}

func stateKey() ([]byte, error) {
	raw := os.Getenv(StateKeyEnv)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf(
			"shared_state.encryption is %s but %s is not set — export a key, or drop the encryption field",
			EncryptionAESGCM, StateKeyEnv)
	}
	// A passphrase of any length becomes a 32-byte key. This is a hash,
	// not a password-hardening KDF: it defends a state file against a
	// casual reader, and the field is documented as doing exactly that.
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

func (b *fileBus) Publish(m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if b.key != nil {
		if data, err = b.seal(data); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.f.Write(append(data, '\n')); err != nil {
		return err
	}
	// Each message is flushed as it is written: the point of the file
	// backend is that a workflow killed mid-run still has its history.
	return b.f.Sync()
}

func (b *fileBus) Messages() ([]Message, error) {
	f, err := os.Open(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Message
	sc := bufio.NewScanner(f)
	// Agent outputs are routinely longer than bufio's 64 KB default.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		if b.key != nil {
			plain, err := b.open(raw)
			if err != nil {
				return nil, fmt.Errorf("%s line %d: %w", b.path, line, err)
			}
			raw = plain
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", b.path, line, err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *fileBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.f.Close()
}

// seal encrypts one record as base64(nonce || ciphertext), keeping the
// file line-oriented so a partial write costs one message, not the log.
func (b *fileBus) seal(plain []byte) ([]byte, error) {
	gcm, err := newGCM(b.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(sealed)))
	base64.StdEncoding.Encode(enc, sealed)
	return enc, nil
}

func (b *fileBus) open(record []byte) ([]byte, error) {
	sealed := make([]byte, base64.StdEncoding.DecodedLen(len(record)))
	n, err := base64.StdEncoding.Decode(sealed, record)
	if err != nil {
		return nil, fmt.Errorf("record is not base64 — was this state file written unencrypted?")
	}
	sealed = sealed[:n]

	gcm, err := newGCM(b.key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("record is too short to be encrypted")
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed — %s does not match the key this state was written with", StateKeyEnv)
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
