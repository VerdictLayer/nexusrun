package fleet

import (
	"context"
	"errors"
	"fmt"
)

type Control interface {
	Checkin(context.Context, Checkin) (Desired, error)
}
type Prepare func(context.Context, Enrollment, Release) (passed, total int, code string)
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
	d, err := control.Checkin(ctx, Checkin{hw, current(), s.Pending})
	var conflict *HTTPError
	if errors.As(err, &conflict) && conflict.Status == 409 && s.Pending != nil {
		d, err = control.Checkin(ctx, Checkin{hw, current(), nil})
	}
	if err != nil {
		return Result{Status: "offline"}, err
	}
	s.Pending = nil
	if err = SaveState(home, s); err != nil {
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
	passed, total, code := prepare(ctx, e, r)
	if ctx.Err() != nil {
		code = "cancelled"
	}
	if code != "ok" || total == 0 || passed != total {
		if code == "ok" {
			code = "evaluation"
		}
		s.LastCode = code
		s.Pending = &Evaluation{r.ID, "rejected", passed, total, code}
		if err = SaveState(home, s); err != nil {
			return Result{}, err
		}
		_, reportErr := control.Checkin(ctx, Checkin{hw, current(), s.Pending})
		if reportErr == nil {
			s.Pending = nil
			if err = SaveState(home, s); err != nil {
				return Result{}, err
			}
		}
		return Result{Status: "rejected", ReleaseID: r.ID, Code: code}, fmt.Errorf("candidate release rejected (%s); active release preserved", code)
	}
	s.Pending = &Evaluation{r.ID, "prepared", passed, total, "ok"}
	if err = SaveState(home, s); err != nil {
		return Result{}, err
	}
	// Reauthorize after testing: a failure on another canary, a pause, or a
	// superseding release must not be ignored while this host was evaluating.
	d, err = control.Checkin(ctx, Checkin{hw, current(), s.Pending})
	if err != nil {
		return Result{Status: "waiting", ReleaseID: r.ID}, err
	}
	if !d.Activate || d.Release == nil || d.Release.ID != r.ID || d.Release.Status == "paused" || d.Release.Digest != r.Digest || d.Release.RuntimeID != r.RuntimeID || d.Release.Repository != r.Repository || d.Release.Channel != r.Channel {
		s.Pending = nil
		if err = SaveState(home, s); err != nil {
			return Result{}, err
		}
		return Result{Status: "waiting", ReleaseID: r.ID}, nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	s.Previous = s.Active
	s.Active = &Activation{r, passed, total}
	s.LastCode = ""
	s.Pending = &Evaluation{r.ID, "active", passed, total, "ok"}
	if err = SaveState(home, s); err != nil {
		return Result{}, err
	}
	_, err = control.Checkin(ctx, Checkin{hw, current(), s.Pending})
	if err != nil {
		return Result{Status: "active", ReleaseID: r.ID, Code: "report_pending"}, nil
	}
	s.Pending = nil
	if err = SaveState(home, s); err != nil {
		return Result{}, err
	}
	return Result{Status: "active", ReleaseID: r.ID}, nil
}
