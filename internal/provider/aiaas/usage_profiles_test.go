package aiaas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageProfilesWithTheTreeToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage-profiles" || r.Header.Get("Authorization") != "Bearer tree" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"from": "2026-11-11T00:00:00Z", "to": "2026-11-25T00:00:00Z", "min_keys": 5, "models": [{"alias": "code", "keys": 12, "requests_per_key_hour": {"median": 6, "p90": 20, "samples": 40, "keys": 12}}]}`))
	}))
	defer srv.Close()
	rep, err := NewClient(srv.URL, "tree", 5*time.Second).UsageProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.MinKeys != 5 || len(rep.Models) != 1 || rep.Models[0].RequestsPerKeyHour.P90 != 20 || rep.Models[0].InputTokensPerRequest != nil {
		t.Errorf("report = %+v", rep)
	}
	if _, err := NewClient(srv.URL, "wrong", 5*time.Second).UsageProfiles(context.Background()); err == nil {
		t.Error("a wrong token is an error")
	}
}
