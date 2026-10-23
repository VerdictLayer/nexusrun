// Package fleet coordinates digest-pinned releases through outbound requests.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var runtimePattern = regexp.MustCompile(`^llama\.cpp/[a-zA-Z0-9._-]+/linux/amd64/[a-f0-9]{64}$`)
var repoPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)

type Hardware struct {
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	Cores int    `json:"cores"`
	RAMMB int64  `json:"ramMB"`
}

type Release struct {
	ID          string   `json:"id"`
	Repository  string   `json:"repository"`
	Channel     string   `json:"channel"`
	Digest      string   `json:"digest"`
	RuntimeID   string   `json:"runtimeID"`
	Status      string   `json:"status"`
	PreviousID  *string  `json:"previousID"`
	CanaryHosts []string `json:"canaryHosts"`
	CreatedAt   string   `json:"createdAt"`
}

func (r Release) Validate(repository, channel string) error {
	if !idPattern.MatchString(r.ID) || !digestPattern.MatchString(r.Digest) || !runtimePattern.MatchString(r.RuntimeID) || r.Repository != repository || r.Channel != channel || (r.Status != "canary" && r.Status != "active" && r.Status != "paused") {
		return errors.New("invalid fleet release or assignment")
	}
	return nil
}

type Evaluation struct {
	ReleaseID string `json:"releaseID"`
	Status    string `json:"status"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	Code      string `json:"code"`
}

type Checkin struct {
	Hardware         Hardware     `json:"hardware"`
	CurrentReleaseID *string      `json:"currentReleaseID"`
	Report           *Evaluation  `json:"report,omitempty"`
	Attestation      *Attestation `json:"attestation,omitempty"`
}

type Desired struct {
	Release  *Release `json:"desired"`
	Activate bool     `json:"activate"`
}

type Client struct {
	Base       string
	Org        string
	Repository string
	Token      string
	HTTP       *http.Client
}

// NewClient permits plaintext only on numeric loopback addresses or localhost.
func NewClient(repository, token string) (*Client, error) {
	parts := strings.SplitN(repository, "/", 3)
	if len(parts) != 3 || !labelPattern.MatchString(parts[1]) || !repoPattern.MatchString(parts[2]) {
		return nil, errors.New("repository must be registry/organization/repository, without a tag or digest")
	}
	u, err := url.Parse("https://" + parts[0])
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid registry host")
	}
	host := u.Hostname()
	if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		u.Scheme = "http"
	}
	return &Client{Base: u.String(), Org: parts[1], Repository: parts[2], Token: token, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("fleet request failed (HTTP %d)", e.Status) }

func (c *Client) call(ctx context.Context, method, path string, body, result any, host bool) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, payload)
	if err != nil {
		return errors.New("invalid fleet endpoint")
	}
	if host {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	} else {
		req.SetBasicAuth("fleet", c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("fleet service unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return errors.New("invalid fleet response size")
	}
	if result != nil && json.Unmarshal(b, result) != nil {
		return errors.New("invalid fleet response")
	}
	return nil
}

func (c *Client) Admin(ctx context.Context, method, action string, body, result any) error {
	return c.call(ctx, method, "/v1/fleet/"+c.Org+"/"+action, body, result, false)
}

func (c *Client) Checkin(ctx context.Context, checkin Checkin) (Desired, error) {
	var d Desired
	err := c.call(ctx, http.MethodPost, "/v1/fleet/checkin", checkin, &d, true)
	if err == nil && d.Release != nil {
		err = d.Release.Validate(c.Repository, d.Release.Channel)
	}
	return d, err
}

func ValidName(name string) bool       { return namePattern.MatchString(name) }
func ValidChannel(channel string) bool { return labelPattern.MatchString(channel) }
func ValidDigest(digest string) bool   { return digestPattern.MatchString(digest) }
func ValidRuntime(id string) bool      { return runtimePattern.MatchString(id) }
