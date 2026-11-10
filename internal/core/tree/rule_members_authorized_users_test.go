package tree_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
)

// budgetForMembers creates a budget the caller may request projects under.
func budgetForMembers(t *testing.T, svc *tree.Service, requester common.TokenList) model.Node {
	t.Helper()
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:           model.RootNodeID,
		Kind:               model.KindBudget,
		Name:               "course",
		Reason:             "course budget",
		Limit:              cores(100),
		AdminScope:         common.TokenList{"group:root"},
		EligibleRequesters: requester,
	}, model.UIActor("root@x"), "root@x", common.TokenList{"group:root"})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return budget
}

// Every authorized_users entry makes the reconciler act in Keystone, so the API
// checks what it is handed instead of passing it through.
func TestAuthorizedUsersAreValidated(t *testing.T) {
	defer tree.RequireGroupMembership(true)()
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	requester := common.TokenList{"user:student@x"}
	budget := budgetForMembers(t, svc, requester)
	// The groups the requester belongs to; only those may they add.
	callerTokens := common.TokenList{"user:student@x", dev.DeptCSFaculty, dev.CourseDozentGroup}

	tooMany := make([]common.AuthorizedUser, 0, common.DefaultMaxAuthorizedUsers+1)
	for i := 0; i <= common.DefaultMaxAuthorizedUsers; i++ {
		tooMany = append(tooMany, common.AuthorizedUser{
			Token: fmt.Sprintf("user:p%d@x.example", i),
			Role:  "member",
		})
	}

	cases := []struct {
		name    string
		users   []common.AuthorizedUser
		wantErr string
	}{
		{
			name:  "a known group is accepted",
			users: []common.AuthorizedUser{{Token: dev.DeptCSFaculty, Role: "member"}},
		},
		{
			name:  "a plain user is accepted and lower-cased",
			users: []common.AuthorizedUser{{Token: "user:Someone@X.example", Role: "READER"}},
		},
		{
			name:  "a relation on a known group is accepted",
			users: []common.AuthorizedUser{{Token: dev.CourseDozentGroup, Role: "member"}},
		},
		{
			name:    "an unknown relation on a known group is refused",
			users:   []common.AuthorizedUser{{Token: dev.CourseGroup + "#tutor", Role: "member"}},
			wantErr: "unknown group",
		},
		{
			name:    "a relation on an unknown group is refused",
			users:   []common.AuthorizedUser{{Token: "group:does-not-exist#dozent", Role: "member"}},
			wantErr: "unknown group",
		},
		{
			name:    "an unknown group is refused",
			users:   []common.AuthorizedUser{{Token: "group:does-not-exist", Role: "member"}},
			wantErr: "unknown group",
		},
		{
			name:    "a group the requester does not belong to is refused",
			users:   []common.AuthorizedUser{{Token: dev.RootGroup, Role: "member"}},
			wantErr: "only members of group",
		},
		{
			name:    "an address with a display name is refused",
			users:   []common.AuthorizedUser{{Token: "user:Mallory <victim@x.example>", Role: "member"}},
			wantErr: "not a valid email",
		},
		{
			name:    "an address with a comment is refused",
			users:   []common.AuthorizedUser{{Token: "user:victim@x.example (mallory)", Role: "member"}},
			wantErr: "not a valid email",
		},
		{
			name:    "a malformed address is refused",
			users:   []common.AuthorizedUser{{Token: "user:not-an-address", Role: "member"}},
			wantErr: "not a valid email",
		},
		{
			name:    "an unsupported role is refused",
			users:   []common.AuthorizedUser{{Token: "user:someone@x.example", Role: "superuser"}},
			wantErr: "expected one of",
		},
		{
			name:    "a token without a prefix is refused",
			users:   []common.AuthorizedUser{{Token: "someone@x.example", Role: "member"}},
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
				Kind:            model.KindProject,
				Name:            "project",
				Reason:          "lab work",
				Limit:           cores(1),
				AuthorizedUsers: tc.users,
			}, model.UIActor("student@x"), "student@x", callerTokens)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the request to be accepted, got %v", err)
				}
				got := node.AuthorizedUsers[0]
				if got.Token != strings.ToLower(tc.users[0].Token) {
					t.Errorf("token not normalized: got %q, want %q", got.Token, strings.ToLower(tc.users[0].Token))
				}
				if got.Role != strings.ToLower(tc.users[0].Role) {
					t.Errorf("role not normalized: got %q", got.Role)
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

// A group grants its members access, so a requester adds only groups they belong to; the managers above the project may add any, and an owner editing the list keeps what a manager put there.
func TestAuthorizedUsers_GroupsNeedMembershipOrAManager(t *testing.T) {
	defer tree.RequireGroupMembership(true)()
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	requester := common.TokenList{"user:student@x"}
	budget := budgetForMembers(t, svc, requester)
	rootGroup := []common.AuthorizedUser{{Token: dev.RootGroup, Role: "member"}}

	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "p", Reason: "r", Limit: cores(1), AuthorizedUsers: rootGroup,
	}, model.UIActor("student@x"), "student@x", requester); err == nil || !strings.Contains(err.Error(), "only members of group") {
		t.Fatalf("a student put the root admins' group into a project: %v", err)
	}

	project, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "p", Reason: "r", Limit: cores(1),
	}, model.UIActor("student@x"), "student@x", requester)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.RequestChange(project.ID, tree.ChangeNodeRequest{AuthorizedUsers: &rootGroup}, model.UIActor("student@x"), requester); err == nil || !strings.Contains(err.Error(), "only members of group") {
		t.Fatalf("a student added the root admins' group by a change: %v", err)
	}

	// A manager of the budget may; the owner then edits the list and keeps the group.
	if _, err := svc.RequestChange(project.ID, tree.ChangeNodeRequest{AuthorizedUsers: &rootGroup}, model.UIActor("root@x"), common.TokenList{"user:root@x", "group:root"}); err != nil {
		t.Fatalf("a manager adds a group: %v", err)
	}
	both := append(slices.Clone(rootGroup), common.AuthorizedUser{Token: "user:friend@x.example", Role: "member"})
	if _, err := svc.RequestChange(project.ID, tree.ChangeNodeRequest{AuthorizedUsers: &both}, model.UIActor("student@x"), requester); err != nil {
		t.Fatalf("the owner edits a list a manager extended: %v", err)
	}
}

// While the role provider knows no lecturers of the Dualis courses, anybody may add any existing group (10.11.2026); an unknown group is still refused.
func TestAuthorizedUsers_AnyExistingGroupWhileMembershipIsOff(t *testing.T) {
	defer tree.RequireGroupMembership(false)()
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	requester := common.TokenList{"user:student@x"}
	budget := budgetForMembers(t, svc, requester)
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "p", Reason: "r", Limit: cores(1), AuthorizedUsers: []common.AuthorizedUser{{Token: dev.RootGroup, Role: "member"}},
	}, model.UIActor("student@x"), "student@x", requester); err != nil {
		t.Fatalf("an existing group the requester does not belong to should be accepted for now: %v", err)
	}
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "q", Reason: "r", Limit: cores(1), AuthorizedUsers: []common.AuthorizedUser{{Token: "group:does-not-exist", Role: "member"}},
	}, model.UIActor("student@x"), "student@x", requester); err == nil || !strings.Contains(err.Error(), "unknown group") {
		t.Fatalf("an unknown group must still be refused: %v", err)
	}
}
