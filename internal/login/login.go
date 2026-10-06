// Package login implements the client side of the OAuth 2.0 Device
// Authorization Grant (RFC 8628), which is how `nexus login` signs in on
// machines that have no browser (or whose browser is on another device).
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const grantType = "urn:ietf:params:oauth:grant-type:device_code"

var (
	// ErrExpired means the code timed out before it was approved.
	ErrExpired = errors.New("the code expired before it was approved")
	// ErrDenied means the user refused the request in the browser.
	ErrDenied = errors.New("the request was denied")
)

// Client talks to a registry's device-authorization endpoints.
type Client struct {
	Base  string // e.g. https://registry.nexusrun.dev
	HTTP  *http.Client
	Sleep func(ctx context.Context, d time.Duration) error // injectable for tests
}

// DeviceAuth is the server's answer to a device-code request.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Token is the credential issued once the user approves.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Org         string `json:"org"`
}

type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) (int, *oauthError, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Base, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= 400 {
		var oe oauthError
		if json.Unmarshal(body, &oe) != nil || oe.Error == "" {
			return resp.StatusCode, nil, fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
		}
		return resp.StatusCode, &oe, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("unexpected response from %s: %w", path, err)
	}
	return resp.StatusCode, nil, nil
}

// Start asks the registry for a device code and a short code for the user.
func (c *Client) Start(ctx context.Context, clientName string) (*DeviceAuth, error) {
	var da DeviceAuth
	_, oe, err := c.post(ctx, "/oauth/device/code", url.Values{"client_name": {clientName}}, &da)
	if err != nil {
		return nil, err
	}
	if oe != nil {
		return nil, fmt.Errorf("could not start sign-in: %s", oe.describe())
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return nil, errors.New("the registry returned an incomplete device code")
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	return &da, nil
}

func (e *oauthError) describe() string {
	if e.Description != "" {
		return e.Description
	}
	return e.Error
}

// Poll waits for the user to approve, honouring the server's polling interval
// and slow_down requests, until a token is issued, the code expires, the user
// denies it, or ctx is cancelled.
func (c *Client) Poll(ctx context.Context, da *DeviceAuth) (*Token, error) {
	wait := c.Sleep
	if wait == nil {
		wait = sleep
	}
	interval := time.Duration(da.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	for {
		if err := wait(ctx, interval); err != nil {
			return nil, err
		}
		if da.ExpiresIn > 0 && time.Now().After(deadline.Add(10*time.Second)) {
			return nil, ErrExpired
		}
		var tok Token
		_, oe, err := c.post(ctx, "/oauth/token", url.Values{"grant_type": {grantType}, "device_code": {da.DeviceCode}}, &tok)
		if err != nil {
			return nil, err
		}
		if oe == nil {
			if tok.AccessToken == "" {
				return nil, errors.New("the registry approved the login but sent no token")
			}
			return &tok, nil
		}
		switch oe.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return nil, ErrExpired
		case "access_denied":
			if oe.Description != "" {
				return nil, fmt.Errorf("%w: %s", ErrDenied, oe.Description)
			}
			return nil, ErrDenied
		default:
			return nil, fmt.Errorf("sign-in failed: %s", oe.describe())
		}
	}
}
