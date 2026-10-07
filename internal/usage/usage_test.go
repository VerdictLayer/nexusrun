package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, _ := r.BasicAuth()
		if r.URL.Path != "/v1/usage" || pw != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"org":"acme","plan":"developer","readOnly":false,"trialEndsAt":null,"hostLimit":25,
"storage":{"usedBytes":400,"limitBytes":1000,"remainingBytes":600},
"bandwidth":{"period":"2026-10","usedBytes":130,"includedBytes":100,"remainingBytes":0,"overageBytes":30,"overageBilled":true}}`))
	}))
	defer srv.Close()

	r, err := Fetch(context.Background(), nil, srv.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	if r.Org != "acme" || r.Storage.Remaining != 600 || r.Bandwidth.Overage != 30 || !r.Bandwidth.OverageBilled {
		t.Fatalf("unexpected report: %+v", r)
	}
	if r.Bandwidth.Included == nil || *r.Bandwidth.Included != 100 {
		t.Fatalf("included = %v", r.Bandwidth.Included)
	}
	if _, err := Fetch(context.Background(), nil, srv.URL, "bad"); err != ErrUnauthorized {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}
