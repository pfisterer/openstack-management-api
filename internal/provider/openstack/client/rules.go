package osclient

import "strings"

// DefaultMaxMembers caps how many members a project of OpenStack may list (Rules). Each entry costs the reconciler Keystone work on every run: a group plus a FindOrCreateUser per member of that group. Real projects have a handful; a course or department goes in as ONE group token, which is why the ceiling can sit far above normal use and still bound the work a single request creates.
const DefaultMaxMembers = 32

// Rules are OpenStack's rules for the tree (tree.ProviderRules).
type Rules struct {
	// Members is how many members a project of OpenStack may list.
	Members int
}

func (Rules) Provider() string { return Provider }

func (r Rules) MaxMembers() int {
	if r.Members <= 0 {
		return DefaultMaxMembers
	}
	return r.Members
}

// OwnerKeystoneRole is the Keystone role a project owner receives.
//
// It is "member", NOT "admin". OpenStack's "admin" role is not confined to the project it is granted in: a large part of the default policy checks `role:admin` without looking at scope, so a project-scoped admin can read cloud-wide state — the full hypervisor list, host aggregates, every tenant's resources. Granting it to a project owner hands a student the operator's view of the whole cloud (observed on ha-teststack 2026-08-06).
//
// "member" is what "may run their own project" actually means: create and manage servers, volumes, networks inside the project, and nothing outside it. Owners do not need "admin" to manage participants either — that happens through this platform, not through Horizon.
const OwnerKeystoneRole = "member"

// KeystoneRole is the Keystone role a member of the tree gets for their role (common.Roles): the role of the same name. A role the tree does not offer (any more) becomes OwnerKeystoneRole: rows persist across releases, and an entry saved while "admin" was still selectable would otherwise keep being re-granted on every pass; dropping it instead would silently revoke access rather than reduce it.
func KeystoneRole(treeRole string) string {
	switch strings.ToLower(strings.TrimSpace(treeRole)) {
	case "member":
		return "member"
	case "reader":
		return "reader"
	}
	return OwnerKeystoneRole
}
