package osclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
)

// Nova reads os-simple-tenant-usage bounds as UTC and gophercloud writes them without a zone, so a day's bounds in Europe/Berlin must go out as UTC. On 12.11.2026 they went out as Berlin wall time: the end lay an hour in the future when the day was collected, Nova cut it to now, and the stored day was short by most of an hour.
func TestTenantUsagesSendsUTCBounds(t *testing.T) {
	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart, gotEnd = r.URL.Query().Get("start"), r.URL.Query().Get("end")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant_usages": []any{}})
	}))
	defer srv.Close()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	c := &OpenStackClient{Compute: &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{HTTPClient: *srv.Client()}, Endpoint: srv.URL + "/"}}
	start := time.Date(2026, 11, 13, 0, 0, 0, 0, berlin)
	if _, err := c.TenantUsages(start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if gotStart != "2026-11-12T23:00:00" || gotEnd != "2026-11-13T23:00:00" {
		t.Fatalf("bounds sent as %s – %s, want 2026-11-12T23:00:00 – 2026-11-13T23:00:00 (UTC)", gotStart, gotEnd)
	}
}
