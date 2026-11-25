package webserver_test

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
)

// A member reading a project over REST (GET /v1/nodes/{id}, /v1/nodes/mine) gets the member's view: their own entry, a count of the others, no admins, no history (tree.redactForMember); the owner and the root admins get it in full.
func TestMemberPrivacy_REST(t *testing.T) {
	tr := newTestTree(t, mockNodes(), testAccounting)
	h := tr.router()
	ownerTokens := common.TokenList{"user:" + userFaculty, dev.DeptCSFaculty}
	project, err := tr.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: "b_cs_faculty", Kind: model.KindProject, Name: "Privacy lab", Reason: "test", Limit: common.ProjectQuota{"cores": 1},
		AdminScope: common.TokenList{"user:co@example.org"},
		AuthorizedUsers: []common.AuthorizedUser{
			{Token: "user:" + userStudent, Role: "member"},
			{Token: "user:a@example.org", Role: "member"},
			{Token: "user:b@example.org", Role: "member"},
		},
	}, model.UIActor(userFaculty), userFaculty, ownerTokens)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	type view struct {
		ID              string                   `json:"id"`
		AuthorizedUsers []struct{ Token string } `json:"authorized_users"`
		MembersHidden   int                      `json:"members_hidden"`
		AdminScope      []string                 `json:"admin_scope"`
		History         []any                    `json:"history"`
		ViewerRelation  string                   `json:"viewer_relation"`
		Owner           string                   `json:"owner"`
		CreatedBy       string                   `json:"created_by"`
	}
	get := func(user string) view {
		rr := do(t, h, http.MethodGet, "/v1/nodes/"+project.ID, user, nil)
		assertStatus(t, rr, http.StatusOK)
		var v view
		mustDecode(t, rr, &v)
		return v
	}
	member := get(userStudent)
	if len(member.AuthorizedUsers) != 1 || member.AuthorizedUsers[0].Token != "user:"+userStudent || member.MembersHidden != 2 {
		t.Errorf("member: expected only their own entry and 2 hidden, got %+v", member)
	}
	if member.AdminScope != nil || member.History != nil || member.CreatedBy != "" || member.ViewerRelation != "member" || member.Owner != "user:"+userFaculty {
		t.Errorf("member: admins, history and creator must be gone, the owner kept: %+v", member)
	}
	for who, user := range map[string]string{"owner": userFaculty, "root admin": ""} {
		full := get(user)
		if len(full.AuthorizedUsers) != 3 || full.MembersHidden != 0 || len(full.AdminScope) != 1 || len(full.History) == 0 {
			t.Errorf("%s: expected the full project, got %+v", who, full)
		}
	}

	rr := do(t, h, http.MethodGet, "/v1/nodes/mine", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	var mine struct{ Items []view }
	mustDecode(t, rr, &mine)
	seen := false
	for _, n := range mine.Items {
		if n.ID != project.ID {
			continue
		}
		seen = true
		if len(n.AuthorizedUsers) != 1 || n.MembersHidden != 2 || n.AdminScope != nil || n.History != nil {
			t.Errorf("mine: expected the member's view, got %+v", n)
		}
	}
	if !seen {
		t.Error("mine: the member does not see the project")
	}
}
