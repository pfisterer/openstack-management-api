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
