package applogic

import (
	"context"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// ResourceStore is the single persistence interface used by Service.
// Implementations can be PostgreSQL-backed or in-memory for tests.
type ResourceStore interface {

	///-------------------------------------------------------------
	///-------------- Initialization and state checks --------------
	///-------------------------------------------------------------

	// For initial population with mock data
	// Returns true if all resource tables are empty (identities, delegations, requests).
	IsResourceStateEmpty(ctx context.Context) (bool, error)

	// Seeds the resource state with provided identities, delegations, and requests. Used for tests or initial setup.
	SeedResourceState(ctx context.Context, identities []webserver.Identity, delegations []webserver.Delegation, requests []webserver.Request) error

	///-------------------------------------------------------------
	///-------------- Delegation operations --------------
	///-------------------------------------------------------------

	// Returns a delegation by its unique ID, or nil if not found.
	GetDelegationByID(ctx context.Context, id string) (*webserver.Delegation, error)

	// Returns delegations whose parent IDs match the provided list.
	ListDelegationsByParentIDs(ctx context.Context, parentIDs []string, limit, offset int) ([]webserver.Delegation, error)

	// Returns delegations where the delegation_scope contains any of the given userTokens.
	// If requireCanDelegate is true, only delegations with CanDelegate=true are returned.
	GetDelegationsFor(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error)

	// Returns delegations created by any of the user's groups (tokens). Supports pagination.
	GetDelegationsBy(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error)

	// Inserts or updates a delegation in storage.
	UpsertDelegation(ctx context.Context, delegation webserver.Delegation) error

	// Deletes delegations by their IDs.
	DeleteDelegations(ctx context.Context, delegationIDs []string) error

	///-------------------------------------------------------------
	///-------------- Request operations --------------
	///-------------------------------------------------------------

	// Returns a request by its unique ID, or nil if not found.
	GetRequestByID(ctx context.Context, id string) (*webserver.Request, error)

	GetRequestsSponsorableBy(ctx context.Context, delegationScopeTokens []string, limit, offset int) ([]webserver.Request, error)

	// Returns requests created by the user/group (matching their tokens).
	ListRequestsBy(ctx context.Context, userEmail string, limit, offset int) ([]webserver.Request, error)

	// Returns all approved requests funded by any of the given delegation IDs.
	ListApprovedRequestsByDelegationIDs(ctx context.Context, delegationIDs []string) ([]webserver.Request, error)

	// Inserts or updates a request in storage.
	UpsertRequest(ctx context.Context, request webserver.Request) error

	// Clears the funding (FundedBy) for requests funded by any of the given delegation IDs.
	ClearRequestFundingByDelegationIDs(ctx context.Context, delegationIDs []string) error
}
