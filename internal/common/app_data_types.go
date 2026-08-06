package common

import (
	"errors"
	"strings"
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
)

// StorageConfiguration holds configuration for the storage backend.
type StorageConfiguration struct {
	Type             string // e.g., "memory" or "postgres"
	ConnectionString string // e.g., Postgres DSN or empty for memory
	AddMockData      bool   // Whether to add mock data on startup
}

// DefaultPageLimit and MaxPageLimit are shared pagination bounds used by both
// the webserver (parsePagination) and the service layer (normalizePagination).
const (
	DefaultPageLimit = 100
	MaxPageLimit     = 500
)

// UserPrefix and GroupPrefix mark the two kinds of authorization token.
const (
	UserPrefix  = "user:"
	GroupPrefix = "group:"
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

// DefaultMaxAuthorizedUsers caps how many participants one project may list.
//
// Real projects have a handful; a course or department goes in as ONE group
// token, which is why the ceiling can sit far above normal use and still bound
// the work a single request creates: every group entry costs the reconciler a
// Keystone group plus a FindOrCreateUser per member of that group, on every run.
const DefaultMaxAuthorizedUsers = 32

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

// TokenList is an alias for a list of tokens (string).
type TokenList []string

// TokenSet is a set of tokens for O(1) membership tests.
type TokenSet map[string]struct{}

// NewTokenSet builds a TokenSet from a TokenList.
func NewTokenSet(tokens TokenList) TokenSet {
	s := make(TokenSet, len(tokens))
	for _, t := range tokens {
		s[t] = struct{}{}
	}
	return s
}

// Contains reports whether token is in the set.
func (s TokenSet) Contains(token string) bool {
	_, ok := s[token]
	return ok
}

// ContainsAny reports whether any token in the list is in the set.
func (s TokenSet) ContainsAny(tokens TokenList) bool {
	for _, t := range tokens {
		if _, ok := s[t]; ok {
			return true
		}
	}
	return false
}

// UserClaims holds the relevant user information extracted from the ID token.
type UserClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Name              string `json:"name,omitempty"`
}

// ResolveEmail returns the best available email-like identifier from the claims,
// trying Email → PreferredUsername → Subject in order.
func (c *UserClaims) ResolveEmail() string {
	if c == nil {
		return ""
	}
	for _, candidate := range []string{c.Email, c.PreferredUsername, c.Subject} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// Identity represents a user or group in the identity catalog.
type Identity struct {
	ID     string    `json:"id"`
	Label  string    `json:"label"`
	Email  string    `json:"email"`
	Tokens TokenList `json:"tokens"`
}
