// Package checkpoint moves an agent's running state between machines.
//
// A `.state.nx` file is a gzipped tar holding an agent's conversation, its
// memory, and enough provenance to know what it was talking to. The point
// is the robot that reboots between WiFi zones, or the workstation whose
// half-finished session has to continue on a laptop: the agent picks up
// where it left off rather than starting over.
//
// What is deliberately *not* in here is the KV cache. The roadmap raised it
// as an open question and the honest answer turned out to be no: llama.cpp's
// cache is not portable across versions, quantizations, or architectures,
// and this runtime drives backends as subprocesses and HTTP servers, so
// there is no cache handle to capture in the first place. Restoring a
// conversation costs one prompt re-ingest on the first turn and is correct
// everywhere; a cache that silently mismatched would be fast and wrong.
package checkpoint

import (
	"archive/tar"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/session"
	"github.com/verdictlayer/nexusrun/internal/store"
)

const (
	// FormatVersion identifies the checkpoint layout.
	FormatVersion = "nexusrun.dev/state/v1"

	// StateKeyEnv holds the passphrase for an encrypted checkpoint.
	StateKeyEnv = "NEXUS_STATE_KEY"

	// Ext is the conventional file extension.
	Ext = ".state.nx"

	// magic prefixes an encrypted checkpoint so a wrong passphrase and a
	// plain gzip file are distinguishable failures.
	magic = "NEXUSSTATE1\n"
)

// Entries inside the archive.
const (
	fileManifest     = "manifest.json"
	fileConversation = "conversation.jsonl"
	fileMemory       = "memory.json"
	fileContext      = "context.json"
	dirModel         = "model/"
)

// Manifest is the checkpoint's metadata, readable without restoring.
type Manifest struct {
	Version string `json:"version"`

	Unit struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Digest  string `json:"digest,omitempty"`
	} `json:"unit"`

	Model struct {
		Source string `json:"source,omitempty"`
		Digest string `json:"digest,omitempty"`
		// Sealed is set when the weights travel inside this file.
		Sealed    bool   `json:"sealed"`
		SealedAs  string `json:"sealed_as,omitempty"`
		SizeBytes int64  `json:"size_bytes,omitempty"`
	} `json:"model"`

	Runtime struct {
		Backend string `json:"backend,omitempty"`
		Device  string `json:"device,omitempty"`
	} `json:"runtime"`

	Session struct {
		Name     string    `json:"name"`
		Turns    int       `json:"turns"`
		Messages int       `json:"messages"`
		Created  time.Time `json:"created"`
	} `json:"session"`

	CreatedAt  time.Time `json:"created_at"`
	Encryption string    `json:"encryption,omitempty"`

	Size struct {
		ConversationBytes int64 `json:"conversation_bytes"`
		ModelBytes        int64 `json:"model_bytes,omitempty"`
		TotalBytes        int64 `json:"total_bytes"`
	} `json:"size"`
}

// SaveOptions configures writing a checkpoint.
type SaveOptions struct {
	// Encrypt seals the whole archive under NEXUS_STATE_KEY.
	Encrypt bool

	// Seal embeds the model weights, making the checkpoint self-contained
	// for transfer to a machine that cannot fetch them. It is measured in
	// gigabytes, so it is never the default.
	Seal bool

	// ModelPath is the resolved weights, required when Seal is set.
	ModelPath string

	Progress func(format string, args ...any)
}

// Save writes a checkpoint for a session.
func Save(w io.Writer, sess *session.Session, opts SaveOptions) (*Manifest, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}

	m := &Manifest{Version: FormatVersion, CreatedAt: time.Now().UTC()}
	name, version, _ := strings.Cut(sess.Unit, ":")
	m.Unit.Name, m.Unit.Version, m.Unit.Digest = name, version, sess.UnitDigest
	m.Model.Source, m.Model.Digest = sess.Model, sess.ModelDigest
	m.Runtime.Backend, m.Runtime.Device = sess.Backend, sess.Device
	m.Session.Name, m.Session.Turns = sess.Name, sess.Turns
	m.Session.Messages, m.Session.Created = len(sess.Messages), sess.Created

	// The conversation is JSON Lines rather than one array: it is the part
	// a human reads when an agent has gone wrong, and one message per line
	// is greppable and diffable.
	var conversation strings.Builder
	for _, msg := range sess.Messages {
		line, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		conversation.Write(line)
		conversation.WriteByte('\n')
	}
	convBytes := []byte(conversation.String())
	m.Size.ConversationBytes = int64(len(convBytes))

	memory := sess.Memory
	if memory == nil {
		memory = map[string]any{}
	}
	memoryBytes, err := json.MarshalIndent(memory, "", "  ")
	if err != nil {
		return nil, err
	}

	ctx := map[string]any{
		"system":     sess.System,
		"context":    sess.Context,
		"turns":      sess.Turns,
		"tokens_out": sess.TokensOut,
		"updated":    sess.Updated,
	}
	contextBytes, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return nil, err
	}

	var sealedName string
	var sealedSize int64
	if opts.Seal {
		if opts.ModelPath == "" {
			return nil, fmt.Errorf("--seal needs the model weights, but none were resolved for %s", sess.Unit)
		}
		info, serr := os.Stat(opts.ModelPath)
		if serr != nil {
			return nil, fmt.Errorf("seal model: %w", serr)
		}
		sealedName = dirModel + path.Base(opts.ModelPath)
		sealedSize = info.Size()
		m.Model.Sealed, m.Model.SealedAs, m.Model.SizeBytes = true, sealedName, sealedSize
		m.Size.ModelBytes = sealedSize
		logf("sealing %s (%s) into the checkpoint", opts.ModelPath, humanSize(sealedSize))
	}
	if opts.Encrypt {
		m.Encryption = "aes256-gcm"
	}
	m.Size.TotalBytes = m.Size.ConversationBytes + int64(len(memoryBytes)) + int64(len(contextBytes)) + sealedSize

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}

	// Encryption covers the whole archive, manifest included: the manifest
	// names the unit and the model, which is exactly what someone carrying
	// a checkpoint on a USB stick may not want readable.
	//
	// It streams in frames rather than sealing one buffer. A sealed
	// checkpoint is the size of the model — gigabytes — and holding that in
	// memory would put the OOM killer between a Raspberry Pi and the
	// air-gapped transfer this feature exists for.
	var sink io.Writer = w
	var enc *frameWriter
	if opts.Encrypt {
		if _, err := w.Write([]byte(magic)); err != nil {
			return nil, err
		}
		if enc, err = newFrameWriter(w); err != nil {
			return nil, err
		}
		sink = enc
	}

	gz := gzip.NewWriter(sink)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := write(fileManifest, manifestBytes); err != nil {
		return nil, err
	}
	if err := write(fileConversation, convBytes); err != nil {
		return nil, err
	}
	if err := write(fileMemory, memoryBytes); err != nil {
		return nil, err
	}
	if err := write(fileContext, contextBytes); err != nil {
		return nil, err
	}
	if opts.Seal {
		f, oerr := os.Open(opts.ModelPath)
		if oerr != nil {
			return nil, oerr
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{
			Name: sealedName, Mode: 0o600, Size: sealedSize, ModTime: m.CreatedAt, Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, f); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Restored is a checkpoint read back.
type Restored struct {
	Manifest *Manifest
	Session  *session.Session

	// SealedModel is the path the embedded weights were written to, when
	// the checkpoint carried them. Empty otherwise.
	SealedModel string
}

// LoadOptions configures reading.
type LoadOptions struct {
	// ModelDir is where sealed weights are extracted. Required only for a
	// sealed checkpoint.
	ModelDir string

	// MetadataOnly stops after the manifest, for `checkpoint inspect`.
	MetadataOnly bool

	Progress func(format string, args ...any)
}

// Load reads a checkpoint.
func Load(r io.Reader, opts LoadOptions) (*Restored, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Peek for the encryption magic. A checkpoint written with --encrypt
	// and read without a key must say so, rather than failing as corrupt gzip.
	head := make([]byte, len(magic))
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	head = head[:n]

	var body io.Reader
	if string(head) == magic {
		dec, derr := newFrameReader(r)
		if derr != nil {
			return nil, derr
		}
		body = dec
	} else {
		body = io.MultiReader(strings.NewReader(string(head)), r)
	}

	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("not a NexusRun checkpoint: %w", err)
	}
	defer gz.Close()

	out := &Restored{Session: &session.Session{Version: session.FormatVersion}}
	tr := tar.NewReader(gz)
	var contextRaw []byte

	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return nil, nerr
		}
		// A checkpoint is an archive from elsewhere; never let an entry name
		// escape where we are writing.
		clean := path.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
			return nil, fmt.Errorf("checkpoint contains an unsafe path %q", hdr.Name)
		}

		switch {
		case clean == fileManifest:
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return nil, rerr
			}
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("checkpoint manifest: %w", err)
			}
			if m.Version != FormatVersion {
				return nil, fmt.Errorf("checkpoint is format %q, this build reads %q", m.Version, FormatVersion)
			}
			out.Manifest = &m
			if opts.MetadataOnly {
				return out, nil
			}

		case clean == fileConversation:
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return nil, rerr
			}
			for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var msg engine.Message
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					return nil, fmt.Errorf("conversation line %d: %w", i+1, err)
				}
				out.Session.Messages = append(out.Session.Messages, msg)
			}

		case clean == fileMemory:
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return nil, rerr
			}
			_ = json.Unmarshal(data, &out.Session.Memory)

		case clean == fileContext:
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return nil, rerr
			}
			contextRaw = data

		case strings.HasPrefix(clean, dirModel):
			if opts.ModelDir == "" {
				logf("checkpoint carries sealed weights, but no directory was given to extract them into; skipping")
				continue
			}
			if err := os.MkdirAll(opts.ModelDir, 0o755); err != nil {
				return nil, err
			}
			dest := path.Join(opts.ModelDir, path.Base(clean))
			logf("extracting sealed weights to %s (%s)", dest, humanSize(hdr.Size))
			f, cerr := os.Create(dest)
			if cerr != nil {
				return nil, cerr
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
			out.SealedModel = dest
		}
	}

	if out.Manifest == nil {
		return nil, fmt.Errorf("checkpoint has no %s", fileManifest)
	}

	m := out.Manifest
	sess := out.Session
	sess.Name = m.Session.Name
	sess.Unit = m.Unit.Name
	if m.Unit.Version != "" {
		sess.Unit = m.Unit.Name + ":" + m.Unit.Version
	}
	sess.UnitDigest = m.Unit.Digest
	sess.Model, sess.ModelDigest = m.Model.Source, m.Model.Digest
	sess.Backend, sess.Device = m.Runtime.Backend, m.Runtime.Device
	sess.Created, sess.Updated = m.Session.Created, m.CreatedAt
	sess.Turns = m.Session.Turns

	if len(contextRaw) > 0 {
		var c struct {
			System    string `json:"system"`
			Context   int    `json:"context"`
			Turns     int    `json:"turns"`
			TokensOut int    `json:"tokens_out"`
		}
		if err := json.Unmarshal(contextRaw, &c); err == nil {
			sess.System, sess.Context = c.System, c.Context
			sess.Turns, sess.TokensOut = c.Turns, c.TokensOut
		}
	}
	return out, nil
}

// String renders a manifest for `checkpoint inspect`.
func (m *Manifest) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Checkpoint  %s\n", m.Version)
	fmt.Fprintf(&b, "Unit:       %s", m.Unit.Name)
	if m.Unit.Version != "" {
		fmt.Fprintf(&b, ":%s", m.Unit.Version)
	}
	b.WriteString("\n")
	if m.Unit.Digest != "" {
		fmt.Fprintf(&b, "  digest:   %s\n", m.Unit.Digest)
	}
	fmt.Fprintf(&b, "Session:    %s (%d turns, %d messages)\n",
		m.Session.Name, m.Session.Turns, m.Session.Messages)
	fmt.Fprintf(&b, "Created:    %s\n", m.CreatedAt.Local().Format(time.RFC3339))
	if m.Model.Source != "" {
		fmt.Fprintf(&b, "Model:      %s\n", m.Model.Source)
	}
	if m.Model.Sealed {
		fmt.Fprintf(&b, "  sealed:   %s (%s) — this checkpoint is self-contained\n",
			m.Model.SealedAs, humanSize(m.Model.SizeBytes))
	} else if m.Model.Source != "" {
		fmt.Fprintf(&b, "  weights:  resolved on restore, not embedded\n")
	}
	if m.Runtime.Backend != "" {
		fmt.Fprintf(&b, "Last ran:   %s on %s\n", m.Runtime.Backend, strings.ToUpper(m.Runtime.Device))
	}
	if m.Encryption != "" {
		fmt.Fprintf(&b, "Encryption: %s\n", m.Encryption)
	}
	fmt.Fprintf(&b, "Size:       %s conversation", humanSize(m.Size.ConversationBytes))
	if m.Size.ModelBytes > 0 {
		fmt.Fprintf(&b, ", %s weights", humanSize(m.Size.ModelBytes))
	}
	fmt.Fprintf(&b, " (%s total)\n", humanSize(m.Size.TotalBytes))
	return b.String()
}

// --- listing --------------------------------------------------------------

// Stored is a checkpoint found in the store.
type Stored struct {
	Path     string
	Name     string
	Size     int64
	Modified time.Time
	Manifest *Manifest
}

// Dir is where `checkpoint save` writes by default.
func Dir(s *store.Store) string { return path.Join(s.Root, "checkpoints") }

// List returns saved checkpoints, newest first, optionally filtered by the
// session they belong to. A file that cannot be read is listed without its
// manifest rather than hidden — a checkpoint you cannot open is exactly
// what you want to be told about.
func List(s *store.Store, sessionName string) ([]*Stored, error) {
	entries, err := os.ReadDir(Dir(s))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Stored
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), Ext) {
			continue
		}
		full := path.Join(Dir(s), e.Name())
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		st := &Stored{
			Path: full, Name: strings.TrimSuffix(e.Name(), Ext),
			Size: info.Size(), Modified: info.ModTime(),
		}
		if f, oerr := os.Open(full); oerr == nil {
			if res, lerr := Load(f, LoadOptions{MetadataOnly: true}); lerr == nil {
				st.Manifest = res.Manifest
			}
			f.Close()
		}
		if sessionName != "" && (st.Manifest == nil || st.Manifest.Session.Name != sessionName) {
			continue
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// --- crypto ---------------------------------------------------------------

func stateKey() ([]byte, error) {
	raw := os.Getenv(StateKeyEnv)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf(
			"this checkpoint is encrypted, but %s is not set — export the key it was written with",
			StateKeyEnv)
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

// chunkSize is the plaintext per frame. Large enough that framing overhead
// is negligible against a multi-gigabyte sealed checkpoint, small enough
// that peak memory stays flat on a small device.
const chunkSize = 1 << 20

// The encrypted layout after the magic is a sequence of frames:
//
//	[4-byte big-endian length][nonce ‖ ciphertext ‖ tag]
//
// Each frame is independently sealed, so writing and reading are both
// streaming operations. Frames also carry their index as additional
// authenticated data, which is what stops an attacker reordering or
// dropping whole chunks of a checkpoint without the tag failing.

type frameWriter struct {
	w     io.Writer
	gcm   cipher.AEAD
	buf   []byte
	index uint64
}

func newFrameWriter(w io.Writer) (*frameWriter, error) {
	key, err := stateKey()
	if err != nil {
		// Writing has its own phrasing: nothing is encrypted yet.
		return nil, fmt.Errorf("--encrypt needs %s to be set", StateKeyEnv)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &frameWriter{w: w, gcm: gcm, buf: make([]byte, 0, chunkSize)}, nil
}

func (f *frameWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		space := chunkSize - len(f.buf)
		n := min(space, len(p))
		f.buf = append(f.buf, p[:n]...)
		p = p[n:]
		if len(f.buf) == chunkSize {
			if err := f.flush(); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (f *frameWriter) flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	nonce := make([]byte, f.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := f.gcm.Seal(nonce, nonce, f.buf, frameAAD(f.index))

	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
	if _, err := f.w.Write(length[:]); err != nil {
		return err
	}
	if _, err := f.w.Write(sealed); err != nil {
		return err
	}
	f.index++
	f.buf = f.buf[:0]
	return nil
}

func (f *frameWriter) Close() error { return f.flush() }

type frameReader struct {
	r     io.Reader
	gcm   cipher.AEAD
	buf   []byte
	index uint64
	done  bool
}

func newFrameReader(r io.Reader) (*frameReader, error) {
	key, err := stateKey()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &frameReader{r: r, gcm: gcm}, nil
}

func (f *frameReader) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.done {
			return 0, io.EOF
		}
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

func (f *frameReader) next() error {
	var length [4]byte
	if _, err := io.ReadFull(f.r, length[:]); err != nil {
		if err == io.EOF {
			f.done = true
			return io.EOF
		}
		return fmt.Errorf("encrypted checkpoint is truncated")
	}
	size := binary.BigEndian.Uint32(length[:])
	// A corrupt length must not become a multi-gigabyte allocation.
	if int(size) < f.gcm.NonceSize() || int(size) > chunkSize+f.gcm.NonceSize()+64 {
		return fmt.Errorf("encrypted checkpoint is corrupt (bad frame length)")
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(f.r, frame); err != nil {
		return fmt.Errorf("encrypted checkpoint is truncated")
	}
	plain, err := f.gcm.Open(nil, frame[:f.gcm.NonceSize()], frame[f.gcm.NonceSize():], frameAAD(f.index))
	if err != nil {
		return fmt.Errorf("decrypt failed — %s does not match the key this checkpoint was written with", StateKeyEnv)
	}
	f.index++
	f.buf = plain
	return nil
}

// frameAAD binds a frame to its position in the stream.
func frameAAD(index uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], index)
	return b[:]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// DigestFile returns a file's sha256, used to verify sealed weights.
func DigestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
