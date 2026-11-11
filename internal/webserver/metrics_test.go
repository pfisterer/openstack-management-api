package webserver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

func scrape(t *testing.T, rec webserver.ReconcilerAPI, enabled bool) string {
	t.Helper()
	w := httptest.NewRecorder()
	webserver.MetricsRouter(rec, enabled).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("scrape: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	return w.Body.String()
}

func TestMetrics(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	rec := &fakeReconciler{status: reconciler.Status{LastRunAt: at, LastError: "keystone down", Counters: reconciler.Counters{ProjectsSynced: 7, ServersShelved: 2, PurgesPending: 5}}}
	out := scrape(t, rec, true)
	for _, want := range []string{
		"osmgt_reconciler_enabled 1\n",
		"osmgt_reconciler_connected 1\n",
		"osmgt_reconciler_last_run_timestamp_seconds 1800000000\n",
		"osmgt_reconciler_last_run_failed 1\n",
		"osmgt_reconciler_last_run_projects_synced 7\n",
		"osmgt_reconciler_last_run_servers_shelved 2\n",
		"# TYPE osmgt_loop_panics_total counter\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "purges_pending") {
		t.Errorf("a count that is not part of the status was written:\n%s", out)
	}

	connecting := scrape(t, &fakeReconciler{notReady: true}, true)
	if !strings.Contains(connecting, "osmgt_reconciler_connected 0\n") || !strings.Contains(connecting, "osmgt_reconciler_last_run_timestamp_seconds 0\n") {
		t.Errorf("a reconciler still connecting:\n%s", connecting)
	}
	if off := scrape(t, nil, false); strings.Contains(off, "osmgt_reconciler") {
		t.Errorf("a reconciler switched off has no series:\n%s", off)
	}
}
