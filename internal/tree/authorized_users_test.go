package tree_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// budgetForMembers creates a budget the caller may request projects under.
func budgetForMembers(t *testing.T, svc *tree.Service, requester common.TokenList) tree.Node {
	t.Helper()
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:           tree.RootNodeID,
		Kind:               tree.KindBudget,
		Name:               "course",
		Reason:             "course budget",
		Limit:              cores(100),
		AdminScope:         common.TokenList{"group:root"},
		EligibleRequesters: requester,
	}, "root@x", "root@x", common.TokenList{"group:root"})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return budget
}

// Every authorized_users entry makes the reconciler act in Keystone, so the API
// checks what it is handed instead of passing it through.
func TestAuthorizedUsersAreValidated(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	requester := common.TokenList{"user:student@x"}
	budget := budgetForMembers(t, svc, requester)

	tooMany := make([]common.AuthorizedUser, 0, common.DefaultMaxAuthorizedUsers+1)
	for i := 0; i <= common.DefaultMaxAuthorizedUsers; i++ {
		tooMany = append(tooMany, common.AuthorizedUser{
			Token:         fmt.Sprintf("user:p%d@x.example", i),
			OpenstackRole: "member",
		})
	}

	cases := []struct {
		name    string
		users   []common.AuthorizedUser
		wantErr string
	}{
		{
			name:  "a known group is accepted",
			users: []common.AuthorizedUser{{Token: mockdata.DeptCSFaculty, OpenstackRole: "member"}},
		},
		{
			name:  "a plain user is accepted and lower-cased",
			users: []common.AuthorizedUser{{Token: "user:Someone@X.example", OpenstackRole: "READER"}},
		},
		{
			name:    "an unknown group is refused",
			users:   []common.AuthorizedUser{{Token: "group:does-not-exist", OpenstackRole: "member"}},
			wantErr: "unknown group",
		},
		{
			name:    "a malformed address is refused",
			users:   []common.AuthorizedUser{{Token: "user:not-an-address", OpenstackRole: "member"}},
			wantErr: "not a valid email",
		},
		{
			name:    "an unsupported role is refused",
			users:   []common.AuthorizedUser{{Token: "user:someone@x.example", OpenstackRole: "superuser"}},
			wantErr: "expected one of",
		},
		{
			name:    "a token without a prefix is refused",
			users:   []common.AuthorizedUser{{Token: "someone@x.example", OpenstackRole: "member"}},
			wantErr: "must be a user: or group: token",
		},
		{
			name:    "more entries than the configured maximum are refused",
			users:   tooMany,
			wantErr: "at most",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, err := svc.CreateNode(tree.CreateNodeRequest{
				ParentID:        budget.ID,
				Kind:            tree.KindProject,
				Name:            "project",
				Reason:          "lab work",
				Limit:           cores(1),
				AuthorizedUsers: tc.users,
			}, "student@x", "student@x", requester)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the request to be accepted, got %v", err)
				}
				got := node.AuthorizedUsers[0]
				if got.Token != strings.ToLower(tc.users[0].Token) {
					t.Errorf("token not normalized: got %q, want %q", got.Token, strings.ToLower(tc.users[0].Token))
				}
				if got.OpenstackRole != strings.ToLower(tc.users[0].OpenstackRole) {
					t.Errorf("role not normalized: got %q", got.OpenstackRole)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, request was accepted", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
