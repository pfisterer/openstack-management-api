package tree

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// Service implements the tree domain: one authorization rule (ancestor walk), one
// capacity mechanism (subtree usage rollup), one lifecycle for budgets and leaves.
// Who a caller is and acts as (role switch, impersonation) is not decided here
// but in package identity; the tree is handed the effective tokens.
type Service struct {
	store nodestore.Store
	roles roles.Provider
	// ownerTokens caches people's tokens for the "owner in group" filter.
	ownerTokens *tokenCache

	// catalog is the deployment's resource catalogue. Validation and anything
	// that has to know WHAT a resource is reads it, through resources(): root
	// admins add and withdraw availabilities at runtime (package catalog).
	catalog common.ResourceCatalog

	// countIDs are the resources that take part in arithmetic — summing across
	// siblings, capacity checks, usage roll-ups. Availabilities are absent.
	//
	// Precomputed and kept beside the catalogue rather than filtered at each
	// call site, because it is read on every rollup. Fixed for the service's
	// life: what changes at runtime are availabilities, never quantities. The name is the guard rail:
	// a plain `resourceIDs` was what let an availability slip into a sum, since
	// nothing at the call site said which set it was.
	countIDs []string
	// storageIDs are the count resources that account as storage — what an
	// archived leaf still holds (see Accounting.ChargeArchived).
	storageIDs []string

	rootAdminTokens common.TokenList
	requestTimeout  time.Duration
	// maxAuthorizedUsers bounds how many participants one node may list.
	maxAuthorizedUsers int

	// accounting decides what a budget is billed for. Both switches only ever
	// make the books stricter when on; off is the older, more permissive
	// behaviour, kept so a bad number can be turned off without a release.
	accounting Accounting

	// deletionAllowed says whether a released or archived leaf may be marked
	// for deletion — only where a reconciler is configured to act on it (see
	// SetDeletionAllowed).
	deletionAllowed bool

	// approvalMu serializes the capacity check-then-write critical sections
	// (create with direct/auto approval, approve, reparent, promote) so concurrent
	// approvals cannot both pass the capacity check and over-allocate a budget.
	// Process-level lock — correct for the single-replica deployment; a
	// multi-replica setup would need DB-level row locking instead.
	approvalMu sync.Mutex

	log *zap.SugaredLogger
}

// Accounting holds the two switches that decide what a budget is billed for.
// A struct rather than two bool parameters: side by side in a call they are
// indistinguishable, and swapping them compiles cleanly and silently changes
// every budget in the deployment.
type Accounting struct {
	// ChargeInUse bills max(declared limit, measured OpenStack usage) instead
	// of the declared limit alone. See chargedQuota for why.
	ChargeInUse bool
	// ChargeReleased keeps a released leaf's limit booked against its budget.
	//
	// Releasing is a request, not a deletion: the reconciler tags the project
	// pending-deletion and hands the actual removal to OpenStack and whoever
	// acts on that tag. Until that happens the servers run and the quota is
	// held, so not charging it lets a budget be re-spent on capacity that is
	// still occupied — release everything, request it again, and the same
	// hardware is booked twice.
	//
	// It ends by itself: once the project is really gone the reconciler drops
	// our record entirely (removeReleasedLeavesWithoutProject), so nothing is
	// charged for a project that no longer exists. That cleanup is what makes
	// this switch safe to leave on — without it a released leaf would be
	// charged forever.
	//
	// Off is the pre-2026-08 behaviour: released frees the budget immediately.
	ChargeReleased bool
	// ChargeArchived keeps the storage an archived leaf still holds booked
	// against its own budget: the measured volume gigabytes, nothing else —
	// its servers are shelved, its floating IPs released, and what it was
	// allocated from budgets further up is given back. Off, archiving frees
	// the budget entirely while the volumes stay until the project is deleted.
	//
	// The usage history records the storage either way; this decides only
	// what a budget is billed for.
	ChargeArchived bool
}

// NewService constructs the tree service.
// rootAdminTokens is synchronized into the root node's AdminScope on every
// Bootstrap — the configuration is the source of truth for that one scope.
func NewService(store nodestore.Store, roles roles.Provider, resources []common.Resource, rootAdminTokens common.TokenList, requestTimeout time.Duration, maxAuthorizedUsers int, accounting Accounting, log *zap.SugaredLogger) *Service {
	if store == nil {
		panic("tree.NewService requires a non-nil store")
	}
	if len(resources) == 0 {
		panic("tree.NewService requires at least one configured resource type")
	}
	countIDs := make([]string, 0, len(resources))
	var storageIDs []string
	for _, r := range resources {
		if r.IsCount() {
			countIDs = append(countIDs, r.ID)
			if r.Accounts == common.AccountsStorage {
				storageIDs = append(storageIDs, r.ID)
			}
		}
	}
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	if maxAuthorizedUsers <= 0 {
		maxAuthorizedUsers = common.DefaultMaxAuthorizedUsers
	}
	return &Service{
		store:              store,
		roles:              roles,
		ownerTokens:        newTokenCache(2 * time.Minute),
		catalog:            common.StaticCatalog(resources),
		countIDs:           countIDs,
		storageIDs:         storageIDs,
		rootAdminTokens:    common.CanonicalTokens(rootAdminTokens),
		requestTimeout:     requestTimeout,
		maxAuthorizedUsers: maxAuthorizedUsers,
		accounting:         accounting,
		log:                log,
	}
}

// UseCatalog replaces the catalogue given to NewService with one that changes
// at runtime. Its quantities must be the same as the ones NewService was given.
func (s *Service) UseCatalog(c common.ResourceCatalog) { s.catalog = c }

// resources is the current catalogue.
func (s *Service) resources() []common.Resource { return s.catalog.Resources() }

// newCtx returns a context with the configured request deadline.
func (s *Service) newCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.requestTimeout)
}

// loadNode loads the node an operation acts on; a missing one is ErrNotFound.
func (s *Service) loadNode(ctx context.Context, id string) (*model.Node, error) {
	n, err := s.store.GetNode(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if n == nil {
		return nil, fmt.Errorf("node %w", common.ErrNotFound)
	}
	return n, nil
}

// loadLive is loadNode for an operation that changes a node of the managed
// tree. An import mirrors OpenStack until somebody promotes it — whatever is
// changed here would be overwritten by the next reconcile, and promoting is
// where it gets a budget, an owner and a limit — and a node that is retired or
// rejected is past changing.
func (s *Service) loadLive(ctx context.Context, id string) (*model.Node, error) {
	n, err := s.loadNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if n.Status == model.StatusImported {
		return nil, fmt.Errorf("imported nodes change only by being promoted: %w", common.ErrForbidden)
	}
	if model.IsTerminalStatus(n.Status) {
		return nil, fmt.Errorf("%w: a node in status %q cannot be changed", common.ErrConflict, n.Status)
	}
	return n, nil
}

// parentChainNodes returns the node's ancestor budgets (parent upward, exclusive).
func (s *Service) parentChainNodes(ctx context.Context, node *model.Node) ([]model.Node, error) {
	if node.ParentID == nil {
		return nil, nil
	}
	return s.nodeChain(ctx, *node.ParentID)
}
