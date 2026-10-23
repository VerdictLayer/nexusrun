package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type controlFunc func(context.Context, Checkin) (Desired, error)

func (f controlFunc) Checkin(ctx context.Context, c Checkin) (Desired, error) { return f(ctx, c) }
func fixture(t *testing.T) (string, Release) {
	t.Helper()
	home := t.TempDir()
	e := Enrollment{HostID: "11111111-1111-1111-1111-111111111111", Name: "host-01", Repository: "registry.test/acme/agent", Channel: "production", Token: "nxr_host_" + strings.Repeat("x", 43)}
	if err := SaveEnrollment(home, e); err != nil {
		t.Fatal(err)
	}
	r := Release{ID: "22222222-2222-2222-2222-222222222222", Repository: "agent", Channel: "production", Digest: "sha256:" + strings.Repeat("a", 64), RuntimeID: "llama.cpp/test/linux/amd64/" + strings.Repeat("b", 64), Status: "canary", CanaryHosts: []string{e.HostID}}
	return home, r
}

var hw = Hardware{"linux", "amd64", 4, 8192}

func TestSyncActivationIsAuthorizedAfterEvaluation(t *testing.T) {
	home, r := fixture(t)
	prepared := false
	calls := 0
	control := controlFunc(func(_ context.Context, c Checkin) (Desired, error) {
		calls++
		s, err := LoadState(home)
		if err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if s.Active != nil {
				t.Fatal("activated before evaluation")
			}
		}
		if calls == 2 {
			if !prepared || c.Report == nil || c.Report.Status != "prepared" || s.Active != nil {
				t.Fatal("candidate was activated before authorization")
			}
		}
		if calls == 3 {
			if c.Report == nil || c.Report.Status != "active" || c.CurrentReleaseID == nil || *c.CurrentReleaseID != r.ID || s.Active == nil {
				t.Fatal("missing active acknowledgement")
			}
		}
		return Desired{&r, true}, nil
	})
	result, err := Sync(context.Background(), home, hw, control, func(context.Context, Enrollment, Release) Outcome {
		prepared = true
		return Outcome{Passed: 2, Total: 2, Code: "ok"}
	})
	if err != nil || result.Status != "active" || calls != 3 {
		t.Fatalf("%+v %v (%d checkins)", result, err, calls)
	}
	s, _ := LoadState(home)
	if s.Active.Release.ID != r.ID || s.Pending != nil {
		t.Fatal("invalid active state")
	}
}

func TestRejectedCandidatePreservesLastWorkingRelease(t *testing.T) {
	for _, code := range []string{"artifact", "runtime", "evaluation", "storage", "cancelled"} {
		t.Run(code, func(t *testing.T) {
			home, r := fixture(t)
			old := r
			old.ID = "33333333-3333-3333-3333-333333333333"
			if err := SaveState(home, State{Active: &Activation{Release: old, Passed: 2, Total: 2}}); err != nil {
				t.Fatal(err)
			}
			gotReport := false
			c := controlFunc(func(_ context.Context, q Checkin) (Desired, error) {
				if q.Report != nil {
					gotReport = true
					if q.Report.Status != "rejected" || q.Report.Code != code {
						t.Fatal("incorrect report")
					}
				}
				return Desired{&r, true}, nil
			})
			result, err := Sync(context.Background(), home, hw, c, func(context.Context, Enrollment, Release) Outcome { return Outcome{Passed: 0, Total: 0, Code: code} })
			if err == nil || result.Status != "rejected" || !gotReport {
				t.Fatalf("%+v %v", result, err)
			}
			s, _ := LoadState(home)
			if s.Active.Release.ID != old.ID {
				t.Fatal("rejection changed the active release")
			}
		})
	}
}

func TestChangedOrPausedReleaseCannotActivateAfterEvaluation(t *testing.T) {
	for _, mode := range []string{"paused", "superseded", "wrong-channel", "wrong-runtime"} {
		t.Run(mode, func(t *testing.T) {
			home, r := fixture(t)
			calls := 0
			c := controlFunc(func(context.Context, Checkin) (Desired, error) {
				calls++
				if calls > 1 {
					switch mode {
					case "paused":
						r.Status = "paused"
					case "superseded":
						r.ID = "33333333-3333-3333-3333-333333333333"
					case "wrong-channel":
						r.Channel = "other"
					case "wrong-runtime":
						r.RuntimeID = "llama.cpp/other/linux/amd64/" + strings.Repeat("c", 64)
					}
				}
				return Desired{&r, true}, nil
			})
			_, _ = Sync(context.Background(), home, hw, c, func(context.Context, Enrollment, Release) Outcome { return Outcome{Passed: 2, Total: 2, Code: "ok"} })
			s, _ := LoadState(home)
			if s.Active != nil {
				t.Fatal("changed authorization activated a candidate")
			}
		})
	}
}

func TestOfflineKeepsActiveAndPendingReport(t *testing.T) {
	home, r := fixture(t)
	initial := State{Active: &Activation{Release: r, Passed: 2, Total: 2}, Pending: &Evaluation{r.ID, "active", 2, 2, "ok"}}
	if err := SaveState(home, initial); err != nil {
		t.Fatal(err)
	}
	_, err := Sync(context.Background(), home, hw, controlFunc(func(context.Context, Checkin) (Desired, error) { return Desired{}, errors.New("offline") }), func(context.Context, Enrollment, Release) Outcome {
		t.Fatal("prepared while offline")
		return Outcome{Passed: 0, Total: 0, Code: ""}
	})
	if err == nil {
		t.Fatal("missing offline error")
	}
	s, _ := LoadState(home)
	if s.Active == nil || s.Active.Release.ID != r.ID || s.Pending == nil {
		t.Fatal("offline check damaged local state")
	}
}

func TestActivationReportIsRetriedAfterConnectionLoss(t *testing.T) {
	home, r := fixture(t)
	calls := 0
	c := controlFunc(func(context.Context, Checkin) (Desired, error) {
		calls++
		if calls == 3 {
			return Desired{}, errors.New("offline")
		}
		return Desired{&r, true}, nil
	})
	result, err := Sync(context.Background(), home, hw, c, func(context.Context, Enrollment, Release) Outcome { return Outcome{Passed: 1, Total: 1, Code: "ok"} })
	if err != nil || result.Code != "report_pending" {
		t.Fatalf("%+v %v", result, err)
	}
	s, _ := LoadState(home)
	if s.Active == nil || s.Pending == nil || s.Pending.Status != "active" {
		t.Fatal("lost active report")
	}
	c = controlFunc(func(_ context.Context, q Checkin) (Desired, error) {
		if q.Report == nil || q.Report.Status != "active" {
			t.Fatal("active report was not retried")
		}
		return Desired{&r, true}, nil
	})
	if _, err = Sync(context.Background(), home, hw, c, func(context.Context, Enrollment, Release) Outcome {
		t.Fatal("reevaluated active release")
		return Outcome{Passed: 0, Total: 0, Code: ""}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStateAndEnrollmentArePrivateAndLockExcludesConcurrentSync(t *testing.T) {
	home, _ := fixture(t)
	for _, name := range []string{"enrollment.json"} {
		info, err := os.Stat(filepath.Join(Root(home), name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%v %v", info, err)
		}
	}
	unlock, err := Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if u, err := Lock(home); err == nil {
		u()
		t.Fatal("concurrent writer acquired lock")
	}
	path := filepath.Join(Root(home), "enrollment.json")
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadEnrollment(home); err == nil {
		t.Fatal("read public host credential")
	}
}
