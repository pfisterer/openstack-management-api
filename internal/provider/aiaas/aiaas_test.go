package aiaas

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"go.uber.org/zap"
)

var catalog = []common.Resource{
	{ID: "cores", Provider: "openstack"},
	{ID: "ws-vcpu-hours", Provider: Provider, Area: common.AreaWorkspaces, Kind: common.KindWindow, Window: "30d"},
	{ID: "ws-gpu-hours-l4-3q", Provider: Provider, Area: common.AreaWorkspaces, Kind: common.KindWindow, Window: "30d"},
	{ID: "ws-gpus-l4-3q", Provider: Provider, Area: common.AreaWorkspaces},
	{ID: "llm-mtokens", Provider: Provider, Area: common.AreaLLM, Kind: common.KindWindow, Window: "30d"},
	{ID: "llm-rpm", Provider: Provider, Area: common.AreaLLM},
	{ID: "llm-tpm", Provider: Provider, Area: common.AreaLLM},
	{ID: "ws-storage-gb", Provider: Provider, Area: common.AreaWorkspaces},
	{ID: "llm-model-code", Provider: Provider, Area: common.AreaLLM, Kind: common.KindBool},
	{ID: "llm-model-all-proxy-models", Provider: Provider, Area: common.AreaLLM, Kind: common.KindBool},
}

func TestProjectFor(t *testing.T) {
	end := "2027-03-31"
	n := model.Node{ID: "p_1", Name: "Kurs", Owner: "user:o@dhbw.de", AdminScope: common.TokenList{"group:mgr"},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "group:k", OpenstackRole: "reader"}, {Token: "user:o@dhbw.de"}, {Token: "user:tutor@dhbw.de", OpenstackRole: "member"}}, TerminationDate: &end,
		Limit: common.ProjectQuota{"cores": 4, "ws-vcpu-hours": 1000, "ws-gpu-hours-l4-3q": common.UnlimitedQuota, "ws-gpus-l4-3q": 2, "llm-mtokens": 80}}
	p := ProjectFor(n, catalog)
	if p.Name != "Kurs" || len(p.Members) != 4 || p.Members[0].Role != "manager" || p.Members[1].Role != "manager" || p.Members[2].Token != "group:k" || p.Members[2].Role != "reader" || p.Members[3].Role != "member" {
		t.Errorf("name/members = %q %+v", p.Name, p.Members)
	}
	if p.EndsAt == nil || p.EndsAt.Format(time.DateOnly) != end {
		t.Errorf("end = %v", p.EndsAt)
	}
	if len(p.Budgets) != 2 || p.Budgets[0].Unit != "vcpu_hours" || p.Budgets[0].Amount != 1000 || p.Budgets[1].GPUClass != "l4-3q" || p.Budgets[1].Amount != 0 {
		t.Errorf("budgets = %+v (unlimited GPU hours: amount 0)", p.Budgets)
	}
	if p.GPUsByClass["l4-3q"] != 2 || p.LLMMTokens != 80 || !p.Workspaces {
		t.Errorf("gpus %v, llm %d, workspaces %v", p.GPUsByClass, p.LLMMTokens, p.Workspaces)
	}
	in := InUseFrom(Status{Used: []Use{{Budget: Budget{Unit: "vcpu_hours", Window: "30d", Amount: 1000}, Used: 12.6}, {Budget: Budget{Unit: "llm_mtokens", Window: "30d", Amount: 80}, Used: 7.4}}}, catalog)
	if len(in) != 2 || in["ws-vcpu-hours"] != 13 || in["llm-mtokens"] != 7 {
		t.Errorf("in use = %v (the LLM pool's use counts as the project's llm-mtokens)", in)
	}
}

type fakeService struct {
	projects map[string]Project
	deleted  []string
}

func (f *fakeService) List(context.Context) ([]Status, error) {
	var out []Status
	for node := range f.projects {
		out = append(out, Status{Node: node, Slug: "t-" + node})
	}
	return out, nil
}
func (f *fakeService) Put(_ context.Context, node string, p Project) (Status, error) {
	f.projects[node] = p
	return Status{Node: node, Slug: "t-" + node, Used: []Use{{Budget: Budget{Unit: "vcpu_hours", Window: "30d"}, Used: 5}}}, nil
}
func (f *fakeService) Delete(_ context.Context, node string) error {
	if _, ok := f.projects[node]; !ok {
		return ErrNotFound
	}
	delete(f.projects, node)
	f.deleted = append(f.deleted, node)
	return nil
}

// One pass: approved projects are carried out and bound, a released one ends, a released one to be deleted goes with its leaf, a project without a leaf goes, and an OpenStack-only project is none of this provider's business.
func TestPass(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	svc := tree.NewService(store, nil, catalog, nil, time.Second, 0, tree.Accounting{}, log)
	for _, n := range []model.Node{
		{ID: "p_ws", Kind: model.KindProject, Status: model.StatusApproved, Name: "WS", Limit: common.ProjectQuota{"ws-vcpu-hours": 100}},
		{ID: "p_vm", Kind: model.KindProject, Status: model.StatusApproved, Name: "VM", Limit: common.ProjectQuota{"cores": 4}},
		{ID: "p_end", Kind: model.KindProject, Status: model.StatusReleased, Name: "End", Limit: common.ProjectQuota{"ws-vcpu-hours": 100},
			Bindings: map[string]model.Binding{Provider: {ExternalID: "t-p_end"}}},
		{ID: "p_del", Kind: model.KindProject, Status: model.StatusReleased, Name: "Del", Flags: []string{model.FlagDeleteRequested},
			Limit: common.ProjectQuota{"llm-mtokens": 10}, Bindings: map[string]model.Binding{Provider: {ExternalID: "t-p_del"}}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeService{projects: map[string]Project{"p_del": {}, "p_gone": {}}}
	s := &Syncer{Tree: svc.ProviderSync(Provider), Service: fake, Resources: func() []common.Resource { return catalog }, Log: log,
		now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }}
	res, err := s.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Synced != 1 || res.Ended != 1 || res.Deleted != 1 || res.Removed != 1 || res.Orphans != 1 {
		t.Errorf("result = %+v", res)
	}
	if _, ok := fake.projects["p_vm"]; ok {
		t.Error("an OpenStack-only project was carried out at the AI service")
	}
	if e := fake.projects["p_end"].EndsAt; e == nil || e.Format(time.DateOnly) != "2026-10-11" {
		t.Errorf("released project ends %v, want yesterday", e)
	}
	n, _ := store.GetNode(ctx, "p_ws")
	if b := n.Binding(Provider); b.ExternalID != "t-p_ws" || b.InUse["ws-vcpu-hours"] != 5 {
		t.Errorf("binding = %+v", b)
	}
	if n, _ := store.GetNode(ctx, "p_del"); n != nil {
		t.Error("the leaf whose deletion was requested is still there")
	}
}

// Rate limits and extra models travel with the project: granted availabilities llm-model-<group> become model groups, an unlimited rate a very large one (0 would mean "default" there).
func TestProjectForLLMLimits(t *testing.T) {
	n := model.Node{ID: "p_1", Name: "Forschung", Owner: "user:o@dhbw.de",
		Limit: common.ProjectQuota{"llm-mtokens": 100, "llm-rpm": 300, "llm-tpm": common.UnlimitedQuota, "llm-model-code": 1, "llm-model-all-proxy-models": 0}}
	p := ProjectFor(n, catalog)
	if p.LLMRPM != 300 || p.LLMTPM != unlimitedRate {
		t.Errorf("rates = %d / %d", p.LLMRPM, p.LLMTPM)
	}
	if len(p.LLMModels) != 1 || p.LLMModels[0] != "code" {
		t.Errorf("models = %v", p.LLMModels)
	}
	// LLM only: no workspace project.
	if p.Workspaces {
		t.Errorf("an LLM project is no workspace project")
	}
	if p := ProjectFor(model.Node{ID: "p_2", Limit: common.ProjectQuota{"llm-mtokens": 5}}, catalog); p.LLMRPM != 0 || p.LLMTPM != 0 || len(p.LLMModels) != 0 {
		t.Errorf("unset limits should stay 0 / empty: %+v", p)
	}
}

// Workspace storage travels as GB, unlimited as a large finite size; what the volume holds comes back as in-use.
func TestStorage(t *testing.T) {
	if p := ProjectFor(model.Node{ID: "p_1", Limit: common.ProjectQuota{"ws-storage-gb": 50}}, catalog); p.StorageGB != 50 {
		t.Errorf("storage = %d", p.StorageGB)
	}
	if p := ProjectFor(model.Node{ID: "p_1", Limit: common.ProjectQuota{"ws-storage-gb": common.UnlimitedQuota}}, catalog); p.StorageGB != unlimitedStorage {
		t.Errorf("unlimited storage = %d", p.StorageGB)
	}
	in := InUseFrom(Status{Used: []Use{{Budget: Budget{Unit: "storage_gb", Amount: 50}, Used: 2.4}}}, catalog)
	if in["ws-storage-gb"] != 2 {
		t.Errorf("in use = %v", in)
	}
}

// GraceDays after its end a project's Lab/AI part is deleted and not carried out again; before that it carries the day for the warnings. A release before the end counts as the end.
func TestExpiry(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	svc := tree.NewService(store, nil, catalog, nil, time.Second, 0, tree.Accounting{}, log)
	day := func(s string) *string { return &s }
	for _, n := range []model.Node{
		{ID: "p_old", Kind: model.KindProject, Status: model.StatusApproved, Name: "Old", TerminationDate: day("2026-09-20"), Limit: common.ProjectQuota{"ws-storage-gb": 5}},
		{ID: "p_soon", Kind: model.KindProject, Status: model.StatusApproved, Name: "Soon", TerminationDate: day("2026-10-05"), Limit: common.ProjectQuota{"ws-storage-gb": 5}},
		{ID: "p_rel", Kind: model.KindProject, Status: model.StatusReleased, Name: "Released", TerminationDate: day("2027-03-31"), Limit: common.ProjectQuota{"ws-storage-gb": 5},
			History: []model.HistoryEntry{{Timestamp: "2026-09-01T10:00:00Z", StatusTo: model.StatusReleased}}},
		{ID: "p_open", Kind: model.KindProject, Status: model.StatusApproved, Name: "Open", Limit: common.ProjectQuota{"ws-storage-gb": 5}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeService{projects: map[string]Project{"p_old": {}, "p_rel": {}}}
	s := &Syncer{Tree: svc.ProviderSync(Provider), Service: fake, Resources: func() []common.Resource { return catalog }, Log: log, GraceDays: 14,
		now: func() time.Time { return time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) }}
	res, err := s.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Expired != 2 {
		t.Errorf("result = %+v", res)
	}
	for _, gone := range []string{"p_old", "p_rel"} {
		if _, ok := fake.projects[gone]; ok {
			t.Errorf("%s should be deleted", gone)
		}
	}
	// Ended 5 Oct: deleted on 20 Oct, announced until then.
	if at := fake.projects["p_soon"].DeleteAt; at == nil || at.Format(time.DateOnly) != "2026-10-20" {
		t.Errorf("p_soon delete_at = %v", at)
	}
	if fake.projects["p_open"].DeleteAt != nil {
		t.Error("a project without an end has no deletion day")
	}
	// Grace 0: nothing is deleted.
	s.GraceDays = 0
	fake.projects["p_old"] = Project{}
	if res, _ := s.Pass(ctx); res.Expired != 0 {
		t.Errorf("grace 0 deleted: %+v", res)
	}
}
