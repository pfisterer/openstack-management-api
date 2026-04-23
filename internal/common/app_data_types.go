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

// Project status constants — single source of truth for all project lifecycle states.
const (
	ProjectStatusPending        = "pending"
	ProjectStatusApproved       = "approved"
	ProjectStatusChangePending  = "change_pending"
	ProjectStatusChangeRejected = "change_rejected"
	ProjectStatusRejected       = "rejected"
	ProjectStatusReleased       = "released"
	// ProjectStatusOpenStackOnly marks a synthetic read-only record that was imported from an
	// OpenStack project that has no matching approved project in storage. These records are
	// managed exclusively by the reconciler and cannot be modified via the API directly —
	// use the promote endpoint to convert them into managed projects.
	ProjectStatusOpenStackOnly = "openstack_only"
)

// Project flag constants for markers that are orthogonal to the status lifecycle.
const (
	// ProjectFlagPromoteOnReconcile is set by the promote API endpoint on an openstack_only
	// record to request that the reconciler adopt the existing OpenStack project on its next
	// run. The reconciler tags the OS project with the resource-id, transitions the record to
	// "pending", and removes this flag. The project then flows through the normal approval cycle.
	ProjectFlagPromoteOnReconcile = "promote_on_reconcile"
)

// TokenEligibilityRule declares which requester tokens may request resources from an owner token.
// An admin who owns a token sets eligible_requesters to control who can make funded project requests
// against that token's pool. ListProjectsManagedBy uses these rules to surface inbound requests.
type TokenEligibilityRule struct {
	// OwnerToken is the token (user: or group:) whose holder manages this rule.
	OwnerToken string `json:"owner_token"`
	// EligibleRequesters is the list of tokens allowed to request resources from OwnerToken.
	EligibleRequesters TokenList `json:"eligible_requesters"`
	CreatedBy          string    `json:"created_by"`
	UpdatedAt          string    `json:"updated_at"`
}

// Delegation represents an admin group that can allocate resources.
type Delegation struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	ParentID           *string `json:"parent_id"`
	CanDelegate        bool    `json:"can_delegate"`
	DelegationStrategy string  `json:"delegation_strategy" binding:"oneof=pool allowance"`
	// AdminScope defines who can see and manage this delegation. Members may approve/reject
	// projects, and (when CanDelegate is true) create sub-delegations. For allowance
	// delegations, AdminScope members are also eligible for auto-approval.
	AdminScope TokenList        `json:"admin_scope"`
	Quota      ProjectResources `json:"quota"`
	CreatedBy  string           `json:"created_by"`
	CreatedAt  string           `json:"created_at"`
	EndDate    *string          `json:"end_date"`
}

// Project represents a resource allocation project.
type Project struct {
	ID              string           `json:"id"`
	Status          string           `json:"status"`
	RequesterTokens TokenList        `json:"requester_tokens"`
	Quota           ProjectQuota     `json:"quota"`
	Reason          string           `json:"reason"`
	FundedBy        *string          `json:"funded_by"`
	Pending         *PendingChanges  `json:"pending"`
	TerminationDate string           `json:"termination_date"`
	AuthorizedUsers []AuthorizedUser `json:"authorized_users"`
	History         []HistoryEntry   `json:"history"`
	// Flags holds orthogonal markers that modify reconciler or API behaviour without
	// changing the primary Status. See ProjectFlag* constants.
	Flags []string `json:"flags,omitempty"`
	// OSProjectID is the OpenStack project ID linked to this project.
	// Set by the reconciler when a project is created or discovered.
	OSProjectID string `json:"os_project_id,omitempty"`
	// OSProjectName is the human-readable name of the linked OpenStack project.
	// Set by the reconciler; only populated for openstack_only records and managed projects
	// that have been synced at least once.
	OSProjectName string `json:"os_project_name,omitempty"`
	// OSOvercommitted is true when the OpenStack project's actual resource usage
	// exceeds the approved quota. Set by the reconciler during each sync cycle.
	// This can occur when quota is reduced below current consumption.
	OSOvercommitted bool `json:"os_overcommitted,omitempty"`
	// ExternalGroupAssignments holds OpenStack group role assignments discovered
	// during import that have no corresponding group: delegation token. The reconciler
	// ensures these groups remain assigned in OpenStack across reconcile cycles without
	// exposing them to the normal delegation-driven group management flow.
	ExternalGroupAssignments []ExternalGroupAssignment `json:"external_group_assignments,omitempty"`
}

// PendingChanges contains proposed modifications to a project.
type PendingChanges struct {
	Quota           *ProjectQuota     `json:"quota,omitempty"`
	TerminationDate *string           `json:"termination_date,omitempty"`
	AuthorizedUsers *[]AuthorizedUser `json:"authorized_users,omitempty"`
}

// AuthorizedUser represents a user/group authorization entry with an OpenStack role.
type AuthorizedUser struct {
	Token         string `json:"token"`
	OpenstackRole string `json:"openstack_role"`
}

// ExternalGroupAssignment records an OpenStack group role assignment that has no
// corresponding delegation token in this system. The reconciler preserves these
// assignments verbatim and never removes them, but does not otherwise manage them.
type ExternalGroupAssignment struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name,omitempty"`
	Role      string `json:"role"`
}

// HistoryEntry records a change to a project.
type HistoryEntry struct {
	Timestamp           string        `json:"timestamp"`
	Event               string        `json:"event"`
	Actor               string        `json:"actor"`
	Group               *string       `json:"group,omitempty"`
	StatusFrom          *string       `json:"status_from,omitempty"`
	StatusTo            string        `json:"status_to"`
	QuotaFrom           *ProjectQuota `json:"quota_from,omitempty"`
	QuotaTo             *ProjectQuota `json:"quota_to,omitempty"`
	TerminationDate     *string       `json:"termination_date,omitempty"`
	TerminationDateFrom *string       `json:"termination_date_from,omitempty"`
	TerminationDateTo   *string       `json:"termination_date_to,omitempty"`
	Reason              *string       `json:"reason,omitempty"`
}

// DelegationStrategy constants define how resources are allocated.
const (
	DelegationStrategyPool      = "pool"      // Resources pooled and allocated on-demand
	DelegationStrategyAllowance = "allowance" // Fixed allowance per user/group
)

// UnlimitedQuota is the sentinel value meaning "no cap on this resource".
// Use -1 in ProjectQuota entries to signal that a resource is unlimited.
const UnlimitedQuota = -1

// ManagedProjectStatuses are states where a delegation manager needs to take action.
var ManagedProjectStatuses = []string{ProjectStatusPending, ProjectStatusChangePending}

// ActiveProjectStatuses are states where resources are actively consumed by a project
// within the delegation hierarchy. Used for usage rollup across the delegation tree.
// Note: openstack_only records are excluded because they carry no FundedBy delegation.
var ActiveProjectStatuses = []string{ProjectStatusApproved, ProjectStatusChangePending}

// ReconcilableProjectStatuses are project states the reconciler projects into OpenStack.
// change_pending projects keep their current approved quota active in OpenStack while
// the proposed change awaits manager approval.
var ReconcilableProjectStatuses = []string{ProjectStatusApproved, ProjectStatusChangePending}

// KnownProjectStatuses contains every real project status, excluding the synthetic
// openstack_only status. Used by the reconciler to identify OS projects that are already
// tracked by a project in any state, so they are not re-imported as openstack_only.
var KnownProjectStatuses = []string{
	ProjectStatusPending,
	ProjectStatusApproved,
	ProjectStatusChangePending,
	ProjectStatusChangeRejected,
	ProjectStatusRejected,
	ProjectStatusReleased,
}

// ProjectQuota represents resource limits and usage by resource ID.
type ProjectQuota map[string]int

// StatusUsage groups the aggregated quota and the contributing project IDs
// for a single project status within a delegation.
type StatusUsage struct {
	Quota      ProjectQuota `json:"quota"`
	ProjectIDs []string     `json:"project_ids"`
}

// UsageByStatus maps project status → StatusUsage for that status.
// Key: project status string (e.g. "approved", "change_pending").
type UsageByStatus map[string]StatusUsage

// TotalQuota collapses all status buckets into a single ProjectQuota by summing
// each resource across every status. Use this when the status breakdown is not needed
// and only the overall committed total matters (e.g. capacity enforcement).
func (u UsageByStatus) TotalQuota(resourceIDs []string) ProjectQuota {
	out := make(ProjectQuota)
	for _, statusUsage := range u {
		for _, id := range resourceIDs {
			out[id] += statusUsage.Quota[id]
		}
	}
	return out
}

// UsagePerDelegation maps delegation ID → UsageByStatus for that delegation.
type UsagePerDelegation map[string]UsageByStatus

// ProjectResources contains limits and current usage broken down by project status.
type ProjectResources struct {
	Limit         ProjectQuota  `json:"limit"`
	UsageByStatus UsageByStatus `json:"usage_by_status,omitempty"`
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
