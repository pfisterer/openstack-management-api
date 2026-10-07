package webserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/usage"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// usageRouter is the mock tree with two projects' consumption yesterday: p_001
// below the CS faculty, p_004 in the biology department.
func usageRouter(t *testing.T, prices *usage.Prices) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, nodes := mockdata.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), quotaResources,
		rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, sugar)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	now := time.Now().UTC()
	yesterday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	rows := usage.NewMemoryStore()
	_ = rows.ReplaceDay(context.Background(), yesterday, []usage.Day{
		{NodeID: "p_001", VCPUHours: 10, BudgetPath: []usage.PathEntry{{ID: "b_cs_faculty"}, {ID: "b_dept_cs"}, {ID: tree.RootNodeID}}},
		{NodeID: "p_004", VCPUHours: 5, BudgetPath: []usage.PathEntry{{ID: "b_dept_bio"}, {ID: tree.RootNodeID}}},
	})
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: true,
		Log:     sugar,
		API: webserver.APIConfig{
			Service: svc, ProjectDefinitions: quotaResources, RoleSwitchGroups: rootAdminTokens,
			Usage: rows, UsagePrices: prices,
		},
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  webserver.DummyAuthMiddleware(),
	})
}

func report(t *testing.T, h http.Handler, path, user string) usage.Report {
	t.Helper()
	rec := do(t, h, http.MethodGet, path, user, nil)
	assertStatus(t, rec, http.StatusOK)
	var r usage.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

// A project's usage is for whoever may see the project; a budget's for its
// managers, not for those who only request from it.
func TestUsage_WhoSeesWhat(t *testing.T) {
	h := usageRouter(t, nil)

	if r := report(t, h, "/v1/nodes/p_001/usage", userFaculty); r.VCPUHours != 10 || len(r.Projects) != 1 {
		t.Errorf("owner sees %v vCPU hours in %d projects", r.VCPUHours, len(r.Projects))
	}
	if r := report(t, h, "/v1/nodes/b_cs_faculty/usage", userFaculty); r.VCPUHours != 10 {
		t.Errorf("the faculty's manager sees %v vCPU hours, want only p_001's 10", r.VCPUHours)
	}
	if r := report(t, h, "/v1/nodes/root/usage", userRoot); r.VCPUHours != 15 || r.ValueEUR != nil {
		t.Errorf("root sees %v vCPU hours, value %v", r.VCPUHours, r.ValueEUR)
	}
	// The student may request from the faculty budget, which does not make
	// the others' consumption theirs to see.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/b_cs_faculty/usage", userStudent, nil), http.StatusForbidden)
	if rec := do(t, h, http.MethodGet, "/v1/nodes/b_dept_bio/usage", userFaculty, nil); rec.Code == http.StatusOK {
		t.Error("another department's usage was shown")
	}
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001/usage?from=2026-10-05&to=2026-10-01", userFaculty, nil), http.StatusBadRequest)
}

// The evaluation over everything is the root admins', and only there is a
// value put on it.
func TestUsage_AdminReport(t *testing.T) {
	h := usageRouter(t, &usage.Prices{VCPUHour: 2})
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/usage", userFaculty, nil), http.StatusForbidden)
	r := report(t, h, "/v1/admin/usage", userRoot)
	if len(r.Projects) != 2 || r.ValueEUR == nil || *r.ValueEUR != 30 {
		t.Fatalf("%d projects, value %v", len(r.Projects), r.ValueEUR)
	}
	if r := report(t, h, "/v1/nodes/root/usage", userRoot); r.ValueEUR != nil {
		t.Error("a node's report carries a value")
	}
}
