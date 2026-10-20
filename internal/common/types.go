package common

import (
	"errors"

	"github.com/pfisterer/cloud-self-service-golib/authn"
)

// Sentinel errors returned by service methods.
// Handlers map these to HTTP status codes via webserver.errorToStatus.
var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	// ErrConflict marks a request that was well-formed but arrived too late:
	// the node had already moved to a status in which the operation no longer
	// applies. It is a distinct sentinel because the caller's reaction differs
	// from a validation error — there is nothing to correct in the input, the
	// client's view of the world is simply stale and needs reloading.
	ErrConflict = errors.New("conflict")
	// ErrInternal marks a failure of the service itself — the store, the role provider — rather than of the request. Handlers answer it with 500 and a generic message and log the detail; the caller cannot correct anything, and the detail (SQL, hosts, timeouts) is nothing to show them.
	ErrInternal = errors.New("internal error")
)

// DefaultPageLimit and MaxPageLimit are shared pagination bounds used by both
// the webserver (parsePagination) and the service layer (normalizePagination).
const (
	DefaultPageLimit = 100
	MaxPageLimit     = 500
)

// OpenstackRoles are the roles a project participant can hold. The list is the
// single source for both the /v1/config response (what the UI offers) and the
// validation of authorized_users — otherwise the UI could offer a role the API
// rejects, or the API could accept one Keystone does not know.
//
// "admin" used to be in this list, which made cloud-wide admin a self-service
// choice: whoever files a project request fills in authorized_users, and the
// auto-approval path only weighs quota against the parent budget — it never
// looks at the roles. Someone requesting a project could therefore hand a
// friend the operator's view of the entire cloud with nobody reviewing it.
// See OwnerOpenstackRole for why "admin" is not project-local in OpenStack.
//
// Granting "admin" remains possible, but only as a deliberate act by an
// operator against Keystone — not as an entry in a self-service form.
var OpenstackRoles = []string{"member", "reader"}

// OwnerOpenstackRole is the Keystone role a project owner receives.
//
// It is "member", NOT "admin". OpenStack's "admin" role is not confined to the
// project it is granted in: a large part of the default policy checks
// `role:admin` without looking at scope, so a project-scoped admin can read
// cloud-wide state — the full hypervisor list, host aggregates, every tenant's
// resources. Granting it to a project owner hands a student the operator's view
// of the whole cloud (observed on ha-teststack 2026-08-06).
//
// "member" is what "may run their own project" actually means: create and manage
// servers, volumes, networks inside the project, and nothing outside it. Owners
// do not need "admin" to manage participants either — that happens through this
// platform, not through Horizon.
const OwnerOpenstackRole = "member"

// DefaultMaxAuthorizedUsers caps how many participants one project that involves OpenStack may list.
//
// Real projects have a handful; a course or department goes in as ONE group
// token, which is why the ceiling can sit far above normal use and still bound
// the work a single request creates: every group entry costs the reconciler a
// Keystone group plus a FindOrCreateUser per member of that group, on every run.
const DefaultMaxAuthorizedUsers = 32

// DefaultMaxAuthorizedUsersOther caps the participants of a project without anything in OpenStack (workspaces, LLM). Those members cost no Keystone work, so the cap only bounds the size of one request and of the lists handed to the AI service; a course of 30–40 students listed one by one fits several times over.
const DefaultMaxAuthorizedUsersOther = 200

// ProviderOpenStack is the provider name of OpenStack, as resources (Resource.Provider) and bindings (Node.Bindings) name it.
const ProviderOpenStack = "openstack"

// MaxQuotaValue bounds every count value of a limit, budget or project, unless the catalogue gives a resource a smaller maximum for projects. The amounts are summed across a whole subtree and multiplied by a provider's unit (RAM in MB); without a bound a single request for 9223372036854775807 cores wraps the sum negative and fits every budget. A billion of anything is beyond any real allocation and leaves the sum of a billion of them far from overflowing.
const MaxQuotaValue = 1_000_000_000

// DefaultMaxProjectsPerOwner caps how many live projects one person may hold under one budget through requests (managers creating directly are not counted against it). Without it a budget that auto-approves hands out OpenStack projects without end, each one a Keystone project, its groups and its quotas.
const DefaultMaxProjectsPerOwner = 10

// MaxNameLength and MaxReasonLength bound the free text of a node: what the UI shows, what the reconciler names projects after, and what every listing carries. Counted in characters, not bytes.
const (
	MaxNameLength   = 200
	MaxReasonLength = 4096
)

// MaxHistoryEntries caps the history a node keeps. Beyond it the oldest entries are dropped (Node.HistoryDropped counts them) rather than further changes refused: refusing would also stop the approvals, rejections and reconciler events a node needs, while the history is a record for people, and its last entries are the ones they read.
const MaxHistoryEntries = 200

// UnlimitedQuota is the sentinel value meaning "no cap on this resource".
// Use -1 in budget limits to signal that a resource is unlimited.
const UnlimitedQuota = -1

// ProjectQuota represents resource limits by resource ID.
type ProjectQuota map[string]int

// AuthorizedUser represents a user/group authorization entry with an OpenStack role.
type AuthorizedUser struct {
	Token         string `json:"token"`
	OpenstackRole string `json:"openstack_role"`
}

// ExternalGroupAssignment records an OpenStack group role assignment that has no
// corresponding token in this system. The reconciler preserves these assignments
// verbatim and never removes them, but does not otherwise manage them.
type ExternalGroupAssignment struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name,omitempty"`
	Role      string `json:"role"`
}

// UserClaims holds the relevant user information extracted from the ID token.
//
// An alias rather than a type of its own: the identity of a caller has to mean
// the same thing in every service, so the definition lives in the shared module
// and this name stays for the call sites that read well with it. Claims.Identity
// replaces what used to be ResolveEmail here — same order (email, then
// preferred_username, then sub), named for what it answers rather than for the
// claim it happens to return today.
type UserClaims = authn.Claims

// Identity represents a user or group in the identity catalog.
type Identity struct {
	ID     string    `json:"id"`
	Label  string    `json:"label"`
	Email  string    `json:"email"`
	Tokens TokenList `json:"tokens"`
}
