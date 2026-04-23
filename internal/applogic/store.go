package applogic

import (
	"context"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ProjectStore is the single persistence interface used by Service.
// Implementations can be PostgreSQL-backed or in-memory for tests.
type ProjectStore interface {

	///-------------------------------------------------------------
	///-------------- Initialization and state checks --------------
	///-------------------------------------------------------------

	// For initial population with mock data
	// Returns true if all resource tables are empty (identities, delegations, projects).
	IsProjectStateEmpty(ctx context.Context) (bool, error)

	// Seeds the resource state with provided identities, delegations, projects, and eligibility rules. Used for tests or initial setup.
	SeedProjectState(ctx context.Context, identities []common.Identity, delegations []common.Delegation, projects []common.Project, eligibilityRules []common.TokenEligibilityRule) error

	///-------------------------------------------------------------
	///-------------- Delegation operations --------------
	///-------------------------------------------------------------

	// Returns a delegation by its unique ID, or nil if not found.
	GetDelegationByID(ctx context.Context, id string) (*common.Delegation, error)

	// Returns delegations whose parent IDs match the provided list.
	ListDelegationsByParentIDs(ctx context.Context, parentIDs []string, limit, offset int) ([]common.Delegation, error)

	// Returns delegations where admin_scope contains any of the given userTokens.
	// Used for "delegations delegated to me" and to find which delegations the caller may approve requests for.
	GetDelegationsByAdminScope(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error)

	// Returns delegations whose parent_id matches any of the given tokens.
	// Used for the "delegations I've made" view — child delegations of groups the caller belongs to.
	GetDelegationsByParentTokens(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error)

	// Inserts or updates a delegation in storage.
	UpsertDelegation(ctx context.Context, delegation common.Delegation) error

	// Deletes delegations by their IDs.
	DeleteDelegations(ctx context.Context, delegationIDs []string) error

	///-------------------------------------------------------------
	///-------------- Request operations --------------
	///-------------------------------------------------------------

	// Returns a project by its unique ID, or nil if not found.
	GetProjectByID(ctx context.Context, id string) (*common.Project, error)

	// Returns projects created by the user/group (matching their tokens).
	ListProjectsBy(ctx context.Context, userEmail string, limit, offset int) ([]common.Project, error)

	// Returns projects funded by any of the given delegation IDs whose status is in statuses.
	// Pass common.ActiveProjectStatuses for usage rollup, or common.ManagedProjectStatuses to find projects to approve.
	// limit=0 and offset=0 returns all matching results (used internally for usage rollup).
	GetProjectsByFundedByIDs(ctx context.Context, delegationIDs []string, statuses []string, limit, offset int) ([]common.Project, error)

	// Inserts or updates a project in storage.
	UpsertProject(ctx context.Context, project common.Project) error

	// Returns all projects whose status is in the given list, regardless of requester or funding.
	// Intended for internal use by the reconciler (not filtered by user scope).
	// Pass limit=0, offset=0 to retrieve all matching results.
	ListProjectsByStatus(ctx context.Context, statuses []string, limit, offset int) ([]common.Project, error)

	// Deletes a single project by ID. Used by the reconciler to remove stale openstack_only records.
	// Returns nil if the project does not exist.
	DeleteProject(ctx context.Context, id string) error

	// Clears the funding (FundedBy) for projects funded by any of the given delegation IDs.
	ClearProjectFundingByDelegationIDs(ctx context.Context, delegationIDs []string) error

	///-------------------------------------------------------------
	///-------------- Token eligibility rule operations ------------
	///-------------------------------------------------------------

	// Returns eligibility rules whose owner_token matches any of the given tokens.
	// Used by ListProjectsManagedBy to discover which requester tokens an admin is responsible for.
	GetEligibilityRulesByOwnerTokens(ctx context.Context, ownerTokens []string) ([]common.TokenEligibilityRule, error)

	// Returns all eligibility rules where any of the given tokens appears in eligible_requesters.
	// Used by ListDelegationsEligibleForMe to find which pool delegations a user may request from.
	GetEligibilityRulesByRequesterTokens(ctx context.Context, requesterTokens []string) ([]common.TokenEligibilityRule, error)

	// Inserts or updates an eligibility rule (keyed by OwnerToken).
	UpsertEligibilityRule(ctx context.Context, rule common.TokenEligibilityRule) error

	// Deletes the eligibility rule for the given owner token, if it exists.
	DeleteEligibilityRule(ctx context.Context, ownerToken string) error
}
