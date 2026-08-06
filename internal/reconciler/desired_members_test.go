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

// An authorized user keeps whichever currently-offered role the project chose.
func TestBuildDesiredMembers_AuthorizedUsersKeepTheirRole(t *testing.T) {
	leaf := tree.Node{
		Owner: "user:owner@example.edu",
		AuthorizedUsers: []common.AuthorizedUser{
			{Token: "user:reader@example.edu", OpenstackRole: "reader"},
			{Token: "user:member@example.edu", OpenstackRole: "member"},
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
		"member@example.edu": "member",
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

// Rows written while "admin" was still selectable must not keep granting it on
// every reconcile pass. Validation only guards new writes; this guards old rows.
func TestBuildDesiredMembers_StoredAdminIsClampedDown(t *testing.T) {
	leaf := tree.Node{
		Owner: "user:owner@example.edu",
		AuthorizedUsers: []common.AuthorizedUser{
			{Token: "user:legacy@example.edu", OpenstackRole: "admin"},
			{Token: "user:garbage@example.edu", OpenstackRole: "not-a-role"},
			{Token: "user:cased@example.edu", OpenstackRole: "  Reader "},
		},
	}

	got := map[string]string{}
	for _, m := range buildDesiredMembers(leaf) {
		got[m.Email] = m.RoleName
	}

	if got["legacy@example.edu"] == "admin" {
		t.Error("a stored 'admin' role was passed through to Keystone")
	}
	if got["legacy@example.edu"] != common.OwnerOpenstackRole {
		t.Errorf("legacy admin = %q, want it clamped to %q", got["legacy@example.edu"], common.OwnerOpenstackRole)
	}
	if got["garbage@example.edu"] != common.OwnerOpenstackRole {
		t.Errorf("unknown role = %q, want %q", got["garbage@example.edu"], common.OwnerOpenstackRole)
	}
	// Clamping must not swallow a role that is merely untidy.
	if got["cased@example.edu"] != "reader" {
		t.Errorf("'  Reader ' = %q, want reader", got["cased@example.edu"])
	}
}
