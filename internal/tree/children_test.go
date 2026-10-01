package tree_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// groupsRoles answers a person's tokens from a fixed table and counts the
// questions, so a test can see the cache work.
type groupsRoles struct {
	common.RoleProvider
	tokens map[string]common.TokenList
	asked  int
}

func (r *groupsRoles) GetUserTokens(_ context.Context, claims *common.UserClaims) (common.TokenList, error) {
	r.asked++
	return append(common.TokenList{"user:" + claims.Email}, r.tokens[claims.Email]...), nil
}

func TestListChildrenFilters(t *testing.T) {
	roles := &groupsRoles{tokens: map[string]common.TokenList{
		"anna@x": {"group:standort-ma", "group:standort-ma#studierende"},
		"ben@x":  {"group:standort-ka"},
	}}
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	svc := tree.NewService(store, roles, testResources, common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Studierende", Reason: "test",
		Limit: cores(100), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:anna@x", "user:ben@x", "user:carla@x"},
	}, tree.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	create := func(owner, name string, members ...string) {
		t.Helper()
		var users []common.AuthorizedUser
		for _, m := range members {
			users = append(users, common.AuthorizedUser{Token: m, OpenstackRole: "member"})
		}
		if _, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID, Kind: tree.KindProject, Name: name, Reason: "a project", Limit: cores(1),
			AuthorizedUsers: users,
		}, tree.UIActor(owner), owner, common.TokenList{"user:" + owner}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	create("anna@x", "Web Engineering")
	create("ben@x", "Datenbanken", "user:anna@x")
	create("carla@x", "Web Projekt")

	names := func(f tree.ChildFilter) []string {
		t.Helper()
		page, err := svc.ListChildren(budget.ID, f, rootTokens, 0, 0)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		var out []string
		for _, n := range page.Items {
			out = append(out, n.Name)
		}
		if page.Total != len(out) {
			t.Errorf("total %d, items %d", page.Total, len(out))
		}
		return out
	}

	cases := []struct {
		name string
		f    tree.ChildFilter
		want []string
	}{
		{"text, sorted by name", tree.ChildFilter{Query: "web", Sort: tree.ChildSortName}, []string{"Web Engineering", "Web Projekt"}},
		{"descending", tree.ChildFilter{Query: "web", Sort: tree.ChildSortName, Desc: true}, []string{"Web Projekt", "Web Engineering"}},
		{"access: owner or member", tree.ChildFilter{Group: "user:Anna@x", GroupMode: tree.GroupAccess, Sort: tree.ChildSortName}, []string{"Datenbanken", "Web Engineering"}},
		{"owner holds a relation", tree.ChildFilter{Group: "group:standort-ma#studierende", GroupMode: tree.GroupOwner}, []string{"Web Engineering"}},
		{"owner in a group", tree.ChildFilter{Group: "group:standort-ka", GroupMode: tree.GroupOwner}, []string{"Datenbanken"}},
		{"status", tree.ChildFilter{Statuses: []string{tree.StatusPending}, Sort: tree.ChildSortName}, []string{"Datenbanken", "Web Engineering", "Web Projekt"}},
		{"status that nobody has", tree.ChildFilter{Statuses: []string{tree.StatusReleased}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := names(c.f); !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}

	// The owners' tokens were asked for once each, then served from the cache.
	if roles.asked > 3 {
		t.Errorf("role provider asked %d times for 3 owners", roles.asked)
	}

	if _, err := svc.ListChildren(budget.ID, tree.ChildFilter{Sort: "size"}, rootTokens, 0, 0); err == nil {
		t.Error("an unknown sort key must be refused, not ignored")
	}
}
