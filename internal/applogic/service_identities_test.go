package applogic_test

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/storage"
	"go.uber.org/zap"
)

// fakeRoles is a minimal RoleProvider that returns a fixed set of groups and their
// members, so the fusion test is isolated from the mock dataset. searchErr forces
// SearchGroupTokens to fail, to exercise the best-effort degrade path.
type fakeRoles struct {
	groups    common.TokenList
	members   map[string][]string
	searchErr error
}

func (f fakeRoles) GetUserTokens(_ context.Context, _ *common.UserClaims) (common.TokenList, error) {
	return nil, nil
}
func (f fakeRoles) SearchGroupTokens(_ context.Context, _ string, _ int) (common.TokenList, error) {
	return f.groups, f.searchErr
}
func (f fakeRoles) GetGroupUsers(_ context.Context, g string) ([]string, error) {
	return f.members[g], nil
}

func emailSet(ids []common.Identity) map[string]common.Identity {
	m := map[string]common.Identity{}
	for _, id := range ids {
		m[id.Email] = id
	}
	return m
}

// TestListAssumableIdentitiesFusion verifies the picker fuses seeded identities,
// role-provider staff, and project participants — deduped by email — and that the
// seeded entry's richer label/tokens survive the merge.
func TestListAssumableIdentitiesFusion(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := storage.NewInMemoryProjectStore(log)

	seeded := []common.Identity{
		{ID: "bob", Label: "Bob Seed", Email: "bob@dhbw.de", Tokens: common.TokenList{"group:root-admin"}},
		// carol is also a staff member below → must dedupe, keeping this label.
		{ID: "carol", Label: "Carol Seed", Email: "carol@dhbw.de"},
	}
	projects := []common.Project{{
		ID:              "p1",
		Status:          "approved",
		RequesterTokens: common.TokenList{"user:alice@student.dhbw-mannheim.de", "group:studierende-dhbw-ma"},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:dave@dhbw.de", OpenstackRole: "member"}},
	}}
	if err := store.SeedProjectState(context.Background(), seeded, nil, projects, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	roles := fakeRoles{
		groups:  common.TokenList{"group:studiendekan-wi"},
		members: map[string][]string{"group:studiendekan-wi": {"carol@dhbw.de", "erin@dhbw.de"}},
	}
	svc := applogic.NewService(store, roles, []string{"cores"}, common.TokenList{"group:root-admin"}, 5*time.Second, log)

	ids, err := svc.ListAssumableIdentities()
	if err != nil {
		t.Fatalf("ListAssumableIdentities: %v", err)
	}
	got := emailSet(ids)

	// alice (participant, pattern-covered student), dave (authorized user),
	// erin (staff), bob + carol (seeded) — all present, group token ignored.
	for _, want := range []string{
		"alice@student.dhbw-mannheim.de", "dave@dhbw.de", "erin@dhbw.de", "bob@dhbw.de", "carol@dhbw.de",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected %q in assumable identities, got %v", want, ids)
		}
	}
	if len(ids) != 5 {
		t.Errorf("expected 5 deduped identities, got %d: %v", len(ids), ids)
	}
	// carol appears in both seeded + staff → single entry, seeded label kept.
	if got["carol@dhbw.de"].Label != "Carol Seed" {
		t.Errorf("carol should keep seeded label, got %q", got["carol@dhbw.de"].Label)
	}
}

// TestListAssumableIdentitiesDegrades verifies that a failing role provider does
// not break the picker — it falls back to seeded + participant identities.
func TestListAssumableIdentitiesDegrades(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := storage.NewInMemoryProjectStore(log)
	seeded := []common.Identity{{ID: "bob", Email: "bob@dhbw.de"}}
	projects := []common.Project{{ID: "p1", RequesterTokens: common.TokenList{"user:alice@dhbw.de"}}}
	if err := store.SeedProjectState(context.Background(), seeded, nil, projects, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	roles := fakeRoles{searchErr: context.DeadlineExceeded}
	svc := applogic.NewService(store, roles, []string{"cores"}, common.TokenList{"group:root-admin"}, 5*time.Second, log)

	ids, err := svc.ListAssumableIdentities()
	if err != nil {
		t.Fatalf("expected graceful degrade, got error: %v", err)
	}
	got := emailSet(ids)
	if _, ok := got["alice@dhbw.de"]; !ok {
		t.Errorf("participant alice should survive role-provider outage, got %v", ids)
	}
	if _, ok := got["bob@dhbw.de"]; !ok {
		t.Errorf("seeded bob should survive role-provider outage, got %v", ids)
	}
}

// TestImpersonationAcceptsDerivedIdentity verifies a project participant (never
// seeded) can be impersonated — the validation uses the same fused set as the
// picker — while an unknown email is still rejected.
func TestImpersonationAcceptsDerivedIdentity(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := storage.NewInMemoryProjectStore(log)
	projects := []common.Project{{ID: "p1", RequesterTokens: common.TokenList{"user:alice@dhbw.de"}}}
	if err := store.SeedProjectState(context.Background(), nil, nil, projects, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := applogic.NewService(store, fakeRoles{}, []string{"cores"}, common.TokenList{"group:root-admin"}, 5*time.Second, log)

	if err := svc.SetUserImpersonationForActor("root@dhbw.de", "alice@dhbw.de"); err != nil {
		t.Errorf("expected participant alice to be assumable, got %v", err)
	}
	if err := svc.SetUserImpersonationForActor("root@dhbw.de", "nobody@dhbw.de"); err == nil {
		t.Errorf("expected unknown identity to be rejected")
	}
}
