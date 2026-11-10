package nodestore

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
)

// The NodeQuery contract both stores keep: the in-memory store filters in Go (matchesQuery), the Postgres store in SQL (applyNodeQuery), and every tree test runs against the first. The same cases run against both, Postgres only with OSMGT_TEST_POSTGRES_DSN set (a throwaway database: the test replaces its nodes).

func TestNodeQueryContract_Memory(t *testing.T) {
	testNodeQueryContract(t, NewInMemoryStore(zap.NewNop().Sugar()))
}

func TestNodeQueryContract_Postgres(t *testing.T) {
	dsn := os.Getenv("OSMGT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSMGT_TEST_POSTGRES_DSN not set")
	}
	s, err := NewPostgresStore(dsn, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	testNodeQueryContract(t, s)
}

func testNodeQueryContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	parent := func(id string) *string { return &id }
	nodes := []model.Node{
		{ID: "root", Kind: model.KindBudget, Status: model.StatusApproved, AdminScope: common.TokenList{"group:root"}},
		{ID: "b_1", ParentID: parent("root"), Kind: model.KindBudget, Status: model.StatusApproved, AdminScope: common.TokenList{"group:a"}, EligibleRequesters: common.TokenList{"group:stud"}},
		{ID: "b_2", ParentID: parent("root"), Kind: model.KindBudget, Status: model.StatusPending, AdminScope: common.TokenList{"group:b"}},
		{ID: "p_1", ParentID: parent("b_1"), Kind: model.KindProject, Status: model.StatusApproved, Owner: "user:alice@x", AdminScope: common.TokenList{"user:carol@x"},
			AuthorizedUsers: []common.AuthorizedUser{{Token: "user:bob@x", Role: "member"}}},
		{ID: "p_2", ParentID: parent("b_1"), Kind: model.KindProject, Status: model.StatusReleased, Owner: "user:bob@x"},
		{ID: "p_3", ParentID: parent("b_2"), Kind: model.KindProject, Status: model.StatusPending, Owner: "user:alice@x",
			AuthorizedUsers: []common.AuthorizedUser{{Token: "group:lab", Role: "member"}}},
	}
	if err := s.Seed(ctx, nil, nodes); err != nil {
		t.Fatalf("seed: %v", err)
	}
	all := []string{"b_1", "b_2", "p_1", "p_2", "p_3", "root"}
	for _, c := range []struct {
		name string
		q    NodeQuery
		want []string
	}{
		{"everything", NodeQuery{}, all},
		{"ids, one missing", NodeQuery{IDs: []string{"p_3", "p_1", "nope"}}, []string{"p_1", "p_3"}},
		{"one parent", NodeQuery{ParentIDs: []string{"b_1"}}, []string{"p_1", "p_2"}},
		{"two parents", NodeQuery{ParentIDs: []string{"b_1", "b_2"}}, []string{"p_1", "p_2", "p_3"}},
		{"below the root", NodeQuery{ParentIDs: []string{"root"}}, []string{"b_1", "b_2"}},
		{"kinds", NodeQuery{Kinds: []string{model.KindBudget}}, []string{"b_1", "b_2", "root"}},
		{"statuses", NodeQuery{Statuses: []string{model.StatusApproved, model.StatusReleased}}, []string{"b_1", "p_1", "p_2", "root"}},
		{"owner", NodeQuery{Owner: "user:alice@x"}, []string{"p_1", "p_3"}},
		{"admin scope, any of", NodeQuery{AdminScopeAny: common.TokenList{"group:a", "user:carol@x"}}, []string{"b_1", "p_1"}},
		{"admin scope, none", NodeQuery{AdminScopeAny: common.TokenList{"group:nobody"}}, nil},
		{"eligible", NodeQuery{EligibleAny: common.TokenList{"group:stud", "group:other"}}, []string{"b_1"}},
		{"responsible as owner", NodeQuery{Responsible: &ResponsibleQuery{Owner: "user:bob@x"}}, []string{"p_2"}},
		{"responsible as owner, admin or member", NodeQuery{Responsible: &ResponsibleQuery{Owner: "user:bob@x", AdminAny: common.TokenList{"group:b"}, MemberAny: common.TokenList{"user:bob@x"}}}, []string{"b_2", "p_1", "p_2"}},
		{"responsible as group member", NodeQuery{Responsible: &ResponsibleQuery{Owner: "user:dave@x", MemberAny: common.TokenList{"group:lab"}}}, []string{"p_3"}},
		{"fields and together", NodeQuery{ParentIDs: []string{"b_1", "b_2"}, Statuses: []string{model.StatusApproved, model.StatusPending}, Owner: "user:alice@x"}, []string{"p_1", "p_3"}},
		{"responsible without owner", NodeQuery{Responsible: &ResponsibleQuery{MemberAny: common.TokenList{"user:bob@x"}}}, []string{"p_1"}},
		{"kinds and responsible", NodeQuery{Kinds: []string{model.KindProject}, Responsible: &ResponsibleQuery{Owner: "user:alice@x", AdminAny: common.TokenList{"group:root"}}}, []string{"p_1", "p_3"}},
	} {
		got, err := s.ListNodes(ctx, c.q, 0, 0)
		if err != nil {
			t.Fatalf("%s: list: %v", c.name, err)
		}
		if ids := idsOf(got); !reflect.DeepEqual(ids, c.want) {
			t.Errorf("%s: ListNodes = %v, want %v", c.name, ids, c.want)
		}
		n, err := s.CountNodes(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: count: %v", c.name, err)
		}
		if n != len(c.want) {
			t.Errorf("%s: CountNodes = %d, want %d", c.name, n, len(c.want))
		}
	}

	// Ordered by id and paginated; the count ignores the page.
	page, err := s.ListNodes(ctx, NodeQuery{}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(page); !reflect.DeepEqual(ids, all[1:3]) {
		t.Errorf("page 2 at offset 1 = %v, want %v", ids, all[1:3])
	}
	if page, _ := s.ListNodes(ctx, NodeQuery{}, 0, 5); !reflect.DeepEqual(idsOf(page), all[5:]) {
		t.Errorf("no limit at offset 5 = %v, want %v", idsOf(page), all[5:])
	}

	counts, err := s.CountChildren(ctx, []string{"root", "b_1", "b_2", "p_1"})
	if err != nil {
		t.Fatal(err)
	}
	if counts["root"].Budgets != 2 || counts["root"].Projects != 0 || counts["b_1"].Projects != 2 || counts["b_2"].Projects != 1 || counts["p_1"] != (ChildCounts{}) {
		t.Errorf("CountChildren = %+v", counts)
	}
}

func idsOf(nodes []model.Node) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}
