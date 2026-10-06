package login

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	t        *testing.T
	script   []string // errors returned by /oauth/token in order; "" means success
	polls    atomic.Int32
	gotName  string
	gotGrant string
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.gotName = r.PostForm.Get("client_name")
		_ = json.NewEncoder(w).Encode(DeviceAuth{DeviceCode: "dc", UserCode: "ABCD-EFGH", VerificationURI: "https://app/?activate=1",
			VerificationURIComplete: "https://app/?device=ABCD-EFGH", ExpiresIn: 600, Interval: 5})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.gotGrant = r.PostForm.Get("grant_type")
		if r.PostForm.Get("device_code") != "dc" {
			f.t.Errorf("device_code = %q", r.PostForm.Get("device_code"))
		}
		i := int(f.polls.Add(1)) - 1
		step := f.script[len(f.script)-1]
		if i < len(f.script) {
			step = f.script[i]
		}
		w.Header().Set("Content-Type", "application/json")
		if step == "" {
			_ = json.NewEncoder(w).Encode(Token{AccessToken: "nxr_pat_x", TokenType: "pat", Scope: "push", Org: "acme"})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": step})
	})
	return mux
}

func run(t *testing.T, script ...string) (*Token, error, *fake, []time.Duration) {
	t.Helper()
	f := &fake{t: t, script: script}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var slept []time.Duration
	c := &Client{Base: srv.URL, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	da, err := c.Start(context.Background(), "ann@laptop")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Poll(context.Background(), da)
	return tok, err, f, slept
}

func TestStartSendsTheClientNameAndReturnsCodes(t *testing.T) {
	f := &fake{t: t, script: []string{""}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	da, err := (&Client{Base: srv.URL}).Start(context.Background(), "ann@laptop")
	if err != nil || da.UserCode != "ABCD-EFGH" || da.Interval != 5 || f.gotName != "ann@laptop" {
		t.Fatalf("da=%+v err=%v name=%q", da, err, f.gotName)
	}
}

func TestPollWaitsThenReturnsTheToken(t *testing.T) {
	tok, err, f, slept := run(t, "authorization_pending", "authorization_pending", "")
	if err != nil || tok.AccessToken != "nxr_pat_x" || tok.Org != "acme" {
		t.Fatalf("tok=%+v err=%v", tok, err)
	}
	if f.polls.Load() != 3 || f.gotGrant != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("polls=%d grant=%q", f.polls.Load(), f.gotGrant)
	}
	for _, d := range slept {
		if d != 5*time.Second {
			t.Errorf("slept %v, want the server interval of 5s", d)
		}
	}
}

func TestSlowDownIncreasesTheInterval(t *testing.T) {
	_, err, _, slept := run(t, "slow_down", "slow_down", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second}
	if len(slept) != 3 || slept[0] != want[0] || slept[1] != want[1] || slept[2] != want[2] {
		t.Errorf("sleeps = %v, want %v", slept, want)
	}
}

func TestTerminalErrors(t *testing.T) {
	if _, err, _, _ := run(t, "authorization_pending", "expired_token"); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
	if _, err, _, _ := run(t, "access_denied"); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
	if _, err, _, _ := run(t, "invalid_grant"); err == nil || errors.Is(err, ErrDenied) {
		t.Errorf("invalid_grant should be a plain failure: %v", err)
	}
}

func TestCancellationStopsPolling(t *testing.T) {
	f := &fake{t: t, script: []string{"authorization_pending"}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{Base: srv.URL, Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}
	da, _ := c.Start(ctx, "x")
	if _, err := c.Poll(ctx, da); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestServerErrorsAreReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 502) }))
	defer srv.Close()
	if _, err := (&Client{Base: srv.URL}).Start(context.Background(), "x"); err == nil {
		t.Fatal("expected an error from a 502")
	}
}
