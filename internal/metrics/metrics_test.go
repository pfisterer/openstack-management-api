package metrics

import (
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/promtext"
)

type counts struct {
	ProjectsSynced int `json:"projects_synced"`
	FloatingIPs    int `json:"floating_ips_released"`
	Pending        int `json:"-"`
	untagged       int
}

func TestWrite(t *testing.T) {
	opts := Loop("test_loop", nil)
	opts.OnPanic("boom", nil)
	var w promtext.Writer
	Write(&w, &Run{Enabled: true, Connected: true, LastRunUnix: 1700000000, Counts: counts{ProjectsSynced: 3, FloatingIPs: 2, Pending: 9, untagged: 1}})
	out := string(w.Bytes())
	for _, want := range []string{
		"osmgt_reconciler_enabled 1\n",
		"osmgt_reconciler_connected 1\n",
		"osmgt_reconciler_last_run_timestamp_seconds 1700000000\n",
		"osmgt_reconciler_last_run_failed 0\n",
		"# HELP osmgt_reconciler_last_run_projects_synced What the reconciler's last successful run counted: projects synced.\n",
		"osmgt_reconciler_last_run_projects_synced 3\n",
		"osmgt_reconciler_last_run_floating_ips_released 2\n",
		`osmgt_loop_panics_total{loop="test_loop"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "pending") || strings.Contains(out, "untagged") || strings.Contains(out, " 9\n") {
		t.Errorf("a field without a JSON name was written:\n%s", out)
	}
	var off promtext.Writer
	Write(&off, nil)
	if strings.Contains(string(off.Bytes()), "reconciler") {
		t.Errorf("no reconciler, no reconciler series:\n%s", off.Bytes())
	}
}
