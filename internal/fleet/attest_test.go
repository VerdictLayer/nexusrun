package fleet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// recordFor runs one activation with a host key and returns the record Cloud
// would hand back from the reports this host sent.
func recordFor(t *testing.T) (Record, ed25519.PrivateKey) {
	t.Helper()
	home, r := fixture(t)
	key, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveHostKey(home, key); err != nil {
		t.Fatal(err)
	}
	e, _ := LoadEnrollment(home)
	var last Checkin
	control := controlFunc(func(_ context.Context, c Checkin) (Desired, error) {
		if c.Report != nil {
			if c.Attestation == nil {
				t.Fatal("report sent without its signed statement")
			}
			last = c
		}
		return Desired{&r, true}, nil
	})
	suite := &SuiteOutcome{Digest: "sha256:" + strings.Repeat("d", 64), Cases: []CaseOutcome{{"arithmetic", true}, {"greeting", true}}}
	if _, err = Sync(context.Background(), home, hw, control, func(context.Context, Enrollment, Release) Outcome {
		return Outcome{Passed: 2, Total: 2, Code: "ok", Suite: suite}
	}); err != nil {
		t.Fatal(err)
	}
	if last.Report.Status != "active" {
		t.Fatalf("last report %q, want active", last.Report.Status)
	}
	return Record{Release: r, Reports: []RecordEntry{{
		HostID: e.HostID, HostName: e.Name, PublicKey: EncodePublicKey(key.Public().(ed25519.PublicKey)),
		Status: last.Report.Status, Passed: last.Report.Passed, Total: last.Report.Total, Code: last.Report.Code,
		Statement: last.Attestation.Statement, Signature: last.Attestation.Signature,
	}}}, key
}

func TestSignedReportVerifiesAndCarriesTheSuite(t *testing.T) {
	rec, _ := recordFor(t)
	checks := rec.Verify()
	if len(checks) != 1 || !checks[0].Valid {
		t.Fatalf("%+v", checks)
	}
	st := checks[0].Statement
	if st.Digest != rec.Release.Digest || st.RuntimeID != rec.Release.RuntimeID || st.Suite == nil || len(st.Suite.Cases) != 2 {
		t.Fatalf("statement %+v", st)
	}
	if checks[0].Fingerprint == "" {
		t.Fatal("no key fingerprint")
	}
}

func TestTamperedRecordFailsVerification(t *testing.T) {
	for name, tamper := range map[string]func(*Record, ed25519.PrivateKey){
		"result":   func(r *Record, _ ed25519.PrivateKey) { r.Reports[0].Passed = 1 },
		"artifact": func(r *Record, _ ed25519.PrivateKey) { r.Release.Digest = "sha256:" + strings.Repeat("c", 64) },
		"other-key": func(r *Record, _ ed25519.PrivateKey) {
			k, _ := NewHostKey()
			r.Reports[0].PublicKey = EncodePublicKey(k.Public().(ed25519.PublicKey))
		},
		"statement": func(r *Record, _ ed25519.PrivateKey) {
			b, _ := base64.StdEncoding.DecodeString(r.Reports[0].Statement)
			var st Statement
			_ = json.Unmarshal(b, &st)
			st.Passed = 99
			b, _ = json.Marshal(st)
			r.Reports[0].Statement = base64.StdEncoding.EncodeToString(b)
		},
		// A statement validly signed for a different release cannot be
		// presented as this one's.
		"replayed": func(r *Record, _ ed25519.PrivateKey) { r.Release.ID = "44444444-4444-4444-4444-444444444444" },
		"unsigned": func(r *Record, _ ed25519.PrivateKey) { r.Reports[0].Statement, r.Reports[0].Signature = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			rec, key := recordFor(t)
			tamper(&rec, key)
			if c := rec.Verify(); c[0].Valid {
				t.Fatalf("tampered record verified: %+v", c[0])
			}
		})
	}
}

func TestHostWithoutKeyReportsUnsignedAndCorruptKeyIsAnError(t *testing.T) {
	home, r := fixture(t)
	e, _ := LoadEnrollment(home)
	if a, err := attest(home, e, r, Evaluation{r.ID, "active", 1, 1, "ok"}, nil); a != nil || err != nil {
		t.Fatalf("%v %v", a, err)
	}
	if err := writePrivate(hostKeyPath(home), hostKey{"not-a-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := attest(home, e, r, Evaluation{r.ID, "active", 1, 1, "ok"}, nil); err == nil {
		t.Fatal("unreadable host key fell back to an unsigned report")
	}
}
