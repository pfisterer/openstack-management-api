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

// Roles are the roles a member of a project can hold in the tree: "member" works in the project, "reader" only looks (decided 10.11.2026: the tree's own terms, docs/plan-caps-katalog.md; before, the field was named openstack_role). Each provider says what a role means there: OpenStack grants the Keystone role of the same name, the AI service gives a reader read access only. The list is the single source for both the /v1/config response (what the UI offers) and the validation of authorized_users.
//
// "admin" used to be in this list, which made cloud-wide admin a self-service choice in OpenStack (see osclient.OwnerKeystoneRole): whoever files a project request fills in authorized_users, and the auto-approval path never looks at the roles. Granting "admin" remains possible, but only as a deliberate act by an operator against Keystone — not as an entry in a self-service form.
var Roles = []string{"member", "reader"}

// DefaultMaxAuthorizedUsers caps the members of a project that no provider with rules of its own concerns (tree.ProviderRules); a provider bounds its projects itself, OpenStack far lower, since each member costs it Keystone work on every run. A course or department goes in as ONE group token.
const DefaultMaxAuthorizedUsers = 200

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

// Holds reports whether the quota carries anything of resource id: a quantity above zero, an unlimited one (UnlimitedQuota) or a granted availability. A missing id holds nothing. What the tree shows as available, which provider a project concerns and whether a provider carries a deletion out all read this.
func (q ProjectQuota) Holds(id string) bool {
	v := q[id]
	return v > 0 || v == UnlimitedQuota
}

// Grants reports whether the quota grants availability id: exactly 1. An availability is 0 or 1 (UnlimitedQuota is refused on one), so nothing else grants it, and a budget's -1 never stood for one (decided 10.11.2026). Only for availabilities; for quantities see Holds.
func (q ProjectQuota) Grants(id string) bool { return q[id] == 1 }

// AuthorizedUser represents a user/group authorization entry with an OpenStack role.
type AuthorizedUser struct {
	Token string `json:"token"`
	Role  string `json:"role"`
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
