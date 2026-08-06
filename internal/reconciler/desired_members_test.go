package reconciler

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// The owner's Keystone role used to be a hardcoded "admin". OpenStack's default
// policy checks `role:admin` largely without regard to scope, so that granted a
// project owner the operator's view of the entire cloud — the full hypervisor
// list included. This test exists so the role cannot drift back there unnoticed.
func TestBuildDesiredMembers_OwnerIsNotCloudAdmin(t *testing.T) {
	leaf := tree.Node{Owner: "user:student@example.edu"}

	members := buildDesiredMembers(leaf)

	if len(members) != 1 {
		t.Fatalf("expected exactly one member for a leaf with only an owner, got %d", len(members))
	}
	if got := members[0].Email; got != "student@example.edu" {
		t.Errorf("owner email = %q, want student@example.edu", got)
	}
	if members[0].RoleName == "admin" {
		t.Fatal("owner was granted the cloud-wide 'admin' role; see common.OwnerOpenstackRole")
	}
	if got := members[0].RoleName; got != common.OwnerOpenstackRole {
		t.Errorf("owner role = %q, want %q", got, common.OwnerOpenstackRole)
	}
}

// An authorized user keeps the role the project asked for — including "admin"
// when an operator deliberately chose it. Only the OWNER default changed.
func TestBuildDesiredMembers_AuthorizedUsersKeepTheirRole(t *testing.T) {
	leaf := tree.Node{
		Owner: "user:owner@example.edu",
		AuthorizedUsers: []common.AuthorizedUser{
			{Token: "user:reader@example.edu", OpenstackRole: "reader"},
			{Token: "user:ops@example.edu", OpenstackRole: "admin"},
			// Group tokens have no Keystone equivalent and must be skipped.
			{Token: "group:course-wi", OpenstackRole: "member"},
		},
	}

	got := map[string]string{}
	for _, m := range buildDesiredMembers(leaf) {
		got[m.Email] = m.RoleName
	}

	want := map[string]string{
		"owner@example.edu":  common.OwnerOpenstackRole,
		"reader@example.edu": "reader",
		"ops@example.edu":    "admin",
	}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v (group tokens must be skipped)", got, want)
	}
	for email, role := range want {
		if got[email] != role {
			t.Errorf("role for %s = %q, want %q", email, got[email], role)
		}
	}
}
