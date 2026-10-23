package fleet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatementType names the signed evaluation statement format.
const StatementType = "nexusrun.dev/fleet-evaluation/v1"

// SuiteOutcome is what a host's acceptance run found, without prompts or
// answers: the exact suite file and which of its cases passed.
type SuiteOutcome struct {
	Digest string        `json:"digest"`
	Cases  []CaseOutcome `json:"cases"`
}

type CaseOutcome struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// Statement is one host's signed account of one release: what it tested,
// against which exact artifact and runtime, and what it then did.
type Statement struct {
	Type       string        `json:"type"`
	HostID     string        `json:"hostID"`
	Repository string        `json:"repository"`
	Channel    string        `json:"channel"`
	ReleaseID  string        `json:"releaseID"`
	Digest     string        `json:"digest"`
	RuntimeID  string        `json:"runtimeID"`
	Status     string        `json:"status"`
	Passed     int           `json:"passed"`
	Total      int           `json:"total"`
	Code       string        `json:"code"`
	Suite      *SuiteOutcome `json:"suite,omitempty"`
	Time       string        `json:"time"`
}

// Attestation carries the exact signed bytes, so a verifier never depends on
// re-encoding JSON the same way the host did.
type Attestation struct {
	Statement string `json:"statement"`
	Signature string `json:"signature"`
}

type hostKey struct {
	Seed string `json:"seed"`
}

func hostKeyPath(home string) string { return filepath.Join(Root(home), "host.key") }

// NewHostKey makes the key this host signs its results with. The private half
// never leaves the host; Cloud is given only the public half at enrollment.
func NewHostKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

func SaveHostKey(home string, key ed25519.PrivateKey) error {
	return writePrivate(hostKeyPath(home), hostKey{base64.StdEncoding.EncodeToString(key.Seed())})
}

// LoadHostKey returns os.ErrNotExist for a host enrolled before signing.
func LoadHostKey(home string) (ed25519.PrivateKey, error) {
	var k hostKey
	if err := readPrivate(hostKeyPath(home), &k); err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(k.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid fleet host key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// EncodePublicKey is the form Cloud stores and records carry.
func EncodePublicKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// Fingerprint is a short, comparable name for a public key, in the same form
// as an SSH key fingerprint.
func Fingerprint(publicKey string) string {
	b, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return ""
	}
	sum := sha256.Sum256(b)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// attest signs an evaluation report. A host enrolled before signing existed
// has no key and reports unsigned; a key that exists but cannot be read is an
// error, never a silent fallback to unsigned.
func attest(home string, e Enrollment, r Release, ev Evaluation, suite *SuiteOutcome) (*Attestation, error) {
	key, err := LoadHostKey(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(Statement{
		Type: StatementType, HostID: e.HostID, Repository: e.Repository, Channel: e.Channel,
		ReleaseID: r.ID, Digest: r.Digest, RuntimeID: r.RuntimeID,
		Status: ev.Status, Passed: ev.Passed, Total: ev.Total, Code: ev.Code, Suite: suite,
		Time: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	return &Attestation{base64.StdEncoding.EncodeToString(b), base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))}, nil
}

// Record is a release and every host's latest report on it, as Cloud returns
// it and as `nexus fleet record` saves it.
type Record struct {
	Release Release       `json:"release"`
	Reports []RecordEntry `json:"reports"`
}

type RecordEntry struct {
	HostID    string `json:"hostID"`
	HostName  string `json:"hostName"`
	PublicKey string `json:"publicKey,omitempty"`
	Status    string `json:"status"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	Code      string `json:"code"`
	UpdatedAt string `json:"updatedAt"`
	Statement string `json:"statement,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// EntryCheck is the verdict on one report.
type EntryCheck struct {
	HostID      string     `json:"hostID"`
	HostName    string     `json:"hostName"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Signed      bool       `json:"signed"`
	Valid       bool       `json:"valid"`
	Problem     string     `json:"problem,omitempty"`
	Statement   *Statement `json:"statement,omitempty"`
}

// Verify checks every report's signature against the key recorded for its
// host, and that the signed statement says what the record says: the same
// host, release, artifact, runtime and result.
func (rec Record) Verify() []EntryCheck {
	out := make([]EntryCheck, 0, len(rec.Reports))
	for _, e := range rec.Reports {
		c := EntryCheck{HostID: e.HostID, HostName: e.HostName, Fingerprint: Fingerprint(e.PublicKey)}
		c.Signed = e.Statement != "" || e.Signature != ""
		if !c.Signed {
			c.Problem = "unsigned report"
			out = append(out, c)
			continue
		}
		st, err := verifyEntry(rec.Release, e)
		if err != nil {
			c.Problem = err.Error()
		} else {
			c.Valid, c.Statement = true, st
		}
		out = append(out, c)
	}
	return out
}

func verifyEntry(r Release, e RecordEntry) (*Statement, error) {
	pub, err := base64.StdEncoding.DecodeString(e.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("no valid public key recorded for this host")
	}
	msg, err := base64.StdEncoding.DecodeString(e.Statement)
	if err != nil {
		return nil, errors.New("statement is not base64")
	}
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil || !ed25519.Verify(pub, msg, sig) {
		return nil, errors.New("signature does not match the host's key")
	}
	var st Statement
	if err = json.Unmarshal(msg, &st); err != nil {
		return nil, errors.New("signed statement is not valid JSON")
	}
	var wrong []string
	check := func(field string, ok bool) {
		if !ok {
			wrong = append(wrong, field)
		}
	}
	check("type", st.Type == StatementType)
	check("host", st.HostID == e.HostID)
	check("release", st.ReleaseID == r.ID)
	check("digest", st.Digest == r.Digest)
	check("runtime", st.RuntimeID == r.RuntimeID)
	check("channel", st.Channel == r.Channel)
	check("repository", strings.HasSuffix(st.Repository, "/"+r.Repository))
	check("result", st.Status == e.Status && st.Passed == e.Passed && st.Total == e.Total && st.Code == e.Code)
	if len(wrong) > 0 {
		return &st, fmt.Errorf("signed statement disagrees with the record: %s", strings.Join(wrong, ", "))
	}
	return &st, nil
}
