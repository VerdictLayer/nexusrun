package engine

import (
	"context"
	"net/http"
	"time"
)

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

func newRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}
