package fleet

import (
	"context"
	"errors"
	"fmt"
)

type Control interface {
	Checkin(context.Context, Checkin) (Desired, error)
}

// Outcome is what testing one candidate found. Code is "ok" only when every
// acceptance test passed.
type Outcome struct {
	Passed int
	Total  int
	Code   string
	Suite  *SuiteOutcome
	// Detail says why a candidate failed. It is shown on this host only and
	// never reported: it can name local paths and test output.
	Detail string
}

type Prepare func(context.Context, Enrollment, Release) Outcome
type Result struct {
	Status    string `json:"status"`
	ReleaseID string `json:"releaseID,omitempty"`
	Code      string `json:"code,omitempty"`
}

// Sync changes only the atomic active-release pointer. Candidate preparation
// never writes through the old release's store or launches its scripts/tools.
func Sync(ctx context.Context, home string, hw Hardware, control Control, prepare Prepare) (Result, error) {
	unlock, err := Lock(home)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	return syncLocked(ctx, home, hw, control, prepare)
}

// syncLocked is Sync for a caller that already holds the fleet lock.
func syncLocked(ctx context.Context, home string, hw Hardware, control Control, prepare Prepare) (Result, error) {
	e, err := LoadEnrollment(home)
	if err != nil {
		return Result{}, err
	}
	s, err := LoadState(home)
	if err != nil {
		return Result{}, err
	}
	current := func() *string {
		if s.Active == nil {
			return nil
		}
		return &s.Active.Release.ID
	}
	// pending records a decision and its signed statement together, before
	// it is reported, so a lost connection cannot separate them.
	pending := func(r Release, ev Evaluation, suite *SuiteOutcome) error {
		proof, err := attest(home, e, r, ev, suite)
		if err != nil {
			return err
		}
		s.Pending, s.Proof = &ev, proof
		return SaveState(home, s)
	}
	clear := func() error {
		s.Pending, s.Proof = nil, nil
		return SaveState(home, s)
	}
	report := func() (Desired, error) {
		return control.Checkin(ctx, Checkin{hw, current(), s.Pending, s.Proof})
	}
	d, err := report()
	var conflict *HTTPError
	if errors.As(err, &conflict) && conflict.Status == 409 && s.Pending != nil {
		d, err = control.Checkin(ctx, Checkin{hw, current(), nil, nil})
	}
	if err != nil {
		return Result{Status: "offline"}, err
	}
	if err = clear(); err != nil {
		return Result{}, err
	}
	if d.Release == nil {
		return Result{Status: "waiting"}, nil
	}
	r := *d.Release
	client, err := NewClient(e.Repository, e.Token)
	if err != nil {
		return Result{}, err
	}
	if err = r.Validate(client.Repository, e.Channel); err != nil {
		return Result{}, err
	}
	if s.Active != nil && s.Active.Release.ID == r.ID {
		return Result{Status: "active", ReleaseID: r.ID}, nil
	}
	if !d.Activate || r.Status == "paused" {
		return Result{Status: r.Status, ReleaseID: r.ID}, nil
	}
	out := prepare(ctx, e, r)
	if ctx.Err() != nil {
		out.Code = "cancelled"
	}
	if out.Code != "ok" || out.Total == 0 || out.Passed != out.Total {
		if out.Code == "ok" {
			out.Code = "evaluation"
		}
		s.LastCode = out.Code
		if err = pending(r, Evaluation{r.ID, "rejected", out.Passed, out.Total, out.Code}, out.Suite); err != nil {
			return Result{}, err
		}
		if _, reportErr := report(); reportErr == nil {
			if err = clear(); err != nil {
				return Result{}, err
			}
		}
		why := out.Code
		if out.Detail != "" {
			why += ": " + out.Detail
		}
		return Result{Status: "rejected", ReleaseID: r.ID, Code: out.Code}, fmt.Errorf("candidate release rejected (%s); active release preserved", why)
	}
	if err = pending(r, Evaluation{r.ID, "prepared", out.Passed, out.Total, "ok"}, out.Suite); err != nil {
		return Result{}, err
	}
	// Reauthorize after testing: a failure on another canary, a pause, or a
	// superseding release must not be ignored while this host was evaluating.
	d, err = report()
	if err != nil {
		return Result{Status: "waiting", ReleaseID: r.ID}, err
	}
	if !d.Activate || d.Release == nil || d.Release.ID != r.ID || d.Release.Status == "paused" || d.Release.Digest != r.Digest || d.Release.RuntimeID != r.RuntimeID || d.Release.Repository != r.Repository || d.Release.Channel != r.Channel {
		if err = clear(); err != nil {
			return Result{}, err
		}
		return Result{Status: "waiting", ReleaseID: r.ID}, nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	s.Previous = s.Active
	s.Active = &Activation{r, out.Passed, out.Total, out.Suite}
	s.LastCode = ""
	if err = pending(r, Evaluation{r.ID, "active", out.Passed, out.Total, "ok"}, out.Suite); err != nil {
		return Result{}, err
	}
	if _, err = report(); err != nil {
		return Result{Status: "active", ReleaseID: r.ID, Code: "report_pending"}, nil
	}
	if err = clear(); err != nil {
		return Result{}, err
	}
	return Result{Status: "active", ReleaseID: r.ID}, nil
}
