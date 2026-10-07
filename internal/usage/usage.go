// Package usage reads a NexusRun Cloud account's plan limits and current use
// from GET /v1/usage. It authenticates with the same personal access token
// that push and pull use.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Report mirrors the registry's /v1/usage response.
type Report struct {
	Org       string  `json:"org"`
	Plan      string  `json:"plan"`
	ReadOnly  bool    `json:"readOnly"`
	TrialEnds *string `json:"trialEndsAt"`
	HostLimit *int64  `json:"hostLimit"`
	Storage   Storage `json:"storage"`
	Bandwidth Band    `json:"bandwidth"`
}

// Storage is stored bytes against the plan limit.
type Storage struct {
	Used      int64 `json:"usedBytes"`
	Limit     int64 `json:"limitBytes"`
	Remaining int64 `json:"remainingBytes"`
}

// Band is this month's pull bandwidth. Included and Remaining are nil when the
// plan has no limit.
type Band struct {
	Period        string `json:"period"`
	Used          int64  `json:"usedBytes"`
	Included      *int64 `json:"includedBytes"`
	Remaining     *int64 `json:"remainingBytes"`
	Overage       int64  `json:"overageBytes"`
	OverageBilled bool   `json:"overageBilled"`
}

// ErrUnauthorized means the registry did not accept the token.
var ErrUnauthorized = errors.New("the registry did not accept this login; run `nexus login` again")

// Fetch asks the registry at base (https://host) for the token's usage.
func Fetch(ctx context.Context, hc *http.Client, base, token string) (*Report, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/v1/usage", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("nexus", token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("this registry does not report usage")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage request failed: %s", resp.Status)
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("unreadable usage response: %w", err)
	}
	return &r, nil
}
