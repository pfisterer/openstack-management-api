// Package reconciler implements a two-way sync between the resource tree and OpenStack projects.
//
// Direction 1 — Storage → OpenStack:
//
//	Approved (and change_pending) project leaves are projected as OpenStack projects.
//	The project is created on first encounter and its quota is kept in sync with the
//	approved limit on every subsequent run. For change_pending leaves the current
//	approved limit is used — the proposed change is only applied after a manager
//	approves it in the service layer.
//
// Direction 2 — OpenStack → Storage:
//
//	Projects that carry ManagedProjectTag but have no matching active leaf in storage
//	are imported as synthetic "imported" leaves under the structural "unassigned"
//	node. They are read-only from the API perspective until a root admin promotes
//	them into a real budget.
//
//	If a scope parent is configured (ScopeParentID, or ScopeParentName which is
//	resolved by name and created on demand) the reconciler additionally scans all
//	projects under that parent, treating any project without a resource-id tag as
//	imported (i.e. projects created directly in OpenStack without going through the
//	management UI).
package reconciler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// ReconcilerStore is the minimal storage interface the reconciler requires.
// It is a structural subset of tree.Store, so any tree store satisfies it.
type ReconcilerStore interface {
	ListNodes(ctx context.Context, q tree.NodeQuery, limit, offset int) ([]tree.Node, error)
	UpsertNode(ctx context.Context, n tree.Node) error
	DeleteNodes(ctx context.Context, ids []string) error
	// Every write to a node that existed when the pass started goes through
	// these two: the pass loads its nodes once and then spends seconds per leaf
	// in OpenStack, and writing that early copy back would undo whatever a user
	// changed meanwhile — a change request, an approval, a release.
	UpdateNode(ctx context.Context, id string, fn func(n *tree.Node) error) (bool, error)
	DeleteNodeIf(ctx context.Context, id string, pred func(n tree.Node) bool) (bool, error)
}

// Config holds all tunables for the reconciler.
type Config struct {
	// Interval between automatic reconciliation runs. Default: 5 minutes.
	Interval time.Duration
	// GroupPrefix is prepended to the group token when naming Keystone groups.
	// Example: "managed-" produces the group name "managed-dept_cs_faculty".
	// Projects are NOT prefixed — they carry the node's own name plus its ID
	// (see buildProjectName) and are identified by tag, not by name.
	GroupPrefix string
	// ScopeParentID, when non-empty, makes the reconciler list ALL projects under this
	// OpenStack parent project and import unknown ones as imported leaves.
	// When empty only projects tagged with ManagedProjectTag are considered.
	ScopeParentID string
	// ScopeParentName is the name-based alternative to ScopeParentID. The project is
	// resolved by name on the first reconcile run and created if it does not exist
	// (except in dry-run mode, where the run proceeds unscoped instead).
	// Ignored when ScopeParentID is set.
	ScopeParentName string
	// DryRun prevents any writes to OpenStack or the store; useful for testing.
	DryRun bool

	// ReleasedArchive archives the OpenStack project of a released leaf right
	// away: the project is disabled, its servers are shelved, its floating IPs
	// are released and the quotas that let anything new run are set to zero
	// (see archiveReleasedProject). Everything but the floating IPs can be
	// undone by an admin. Volumes stay until the project is deleted.
	ReleasedArchive bool
	// ReleasedDelete says when the OpenStack project of a released or archived
	// leaf is emptied and deleted: ReleasedDeleteNever (default),
	// ReleasedDeleteOnRequest — when someone asks for it (FlagDeleteRequested),
	// ReleasedDeleteAfterGrace — on request, or once the date in its
	// pending-deletion tag has passed — or ReleasedDeleteImmediately.
	ReleasedDelete string
	// ReleasedDeleteGraceDays is added to the day a project is released to
	// compute the date in its pending-deletion tag — when it is deleted under
	// ReleasedDeleteAfterGrace, and announced otherwise. Default: 30.
	ReleasedDeleteGraceDays int
	// PurgeDNSAndObjectStorage lets emptying a project delete its DNS zones
	// and object storage. Off, they are only reported and keep the project
	// from being deleted. Default: false.
	PurgeDNSAndObjectStorage bool
	// PurgeSkipStages names stages of emptying a project (PurgeStages) that
	// are passed over, for a service that is broken for good: the project is
	// deleted all the same and what the stage would have removed stays
	// behind. Default: none.
	PurgeSkipStages []string
	// DeleteOrphanedUsers deletes Keystone accounts this service created once
	// they hold no project role any more (pruneOrphanedUsers). Default: false —
	// see there for why that is unsafe with a service user whose view is
	// limited to its own domain.
	DeleteOrphanedUsers bool
	// ArchivedTagPrefix marks a project archiveReleasedProject has finished
	// with: "<prefix><YYYY-MM-DD>". Default: "archived:".
	ArchivedTagPrefix string
	// PendingDeletionTagPrefix is the tag prefix for the scheduled deletion date.
	// Full tag format: "<prefix><YYYY-MM-DD>". Default: "pending-deletion:".
	PendingDeletionTagPrefix string
	// TerminationTagPrefix is the tag prefix carrying the leaf's termination date
	// (the stored RFC3339 timestamp, verbatim). Default: "termination:".
	TerminationTagPrefix string
	// ManagedProjectTag marks the projects this service manages. The client
	// writes and filters by it; here it is only reported in the status.
	ManagedProjectTag string
	// StatusTagPrefix is the tag prefix carrying the leaf's lifecycle status, so
	// an outside workflow can select projects by state without asking this API.
	// Full tag format: "<prefix><status>", e.g. "status:released". Default:
	// "status:". Empty switches the tag off.
	StatusTagPrefix string
	// ContactTagPrefix is the prefix for tags that record owner contact addresses.
	// Default: "contact:".
	ContactTagPrefix string
}

// Status is returned by GetStatus to report the outcome of the last reconciliation run.
type Status struct {
	LastRunAt       time.Time `json:"last_run_at"`
	LastError       string    `json:"last_error,omitempty"`
	ProjectsSynced  int       `json:"projects_synced"`
	ProjectsCreated int       `json:"projects_created"`
	ImportedLeaves  int       `json:"imported_leaves"`
	ImportedRemoved int       `json:"imported_removed"`
	// ReleasedLeavesRemoved counts released records dropped because their
	// OpenStack project has been deleted — the end of the handover that the
	// pending-deletion tag starts.
	ReleasedLeavesRemoved     int `json:"released_leaves_removed"`
	OrphanedUsersRemoved      int `json:"orphaned_users_removed"`
	GroupsCreated             int `json:"groups_created"`
	GroupsSynced              int `json:"groups_synced"`
	ProjectsTaggedForDeletion int `json:"projects_tagged_for_deletion"`
	ProjectsArchived          int `json:"projects_archived"`
	ServersShelved            int `json:"servers_shelved"`
	FloatingIPsReleased       int `json:"floating_ips_released"`
	ProjectsDeleted           int `json:"projects_deleted"`
	// ResourcesPurged counts what was deleted inside projects on their way to
	// deletion — servers, volumes, networks and the like.
	ResourcesPurged  int `json:"resources_purged"`
	ProjectsPromoted int `json:"projects_promoted"`
	// ProjectsRetagged counts managed projects whose resource-id tag was missing
	// and had to be restored — see recoverUntaggedProject. Anything above zero
	// means somebody edited tags in OpenStack.
	ProjectsRetagged int  `json:"projects_retagged"`
	Running          bool `json:"running"`
	// PreseedConflicts lists users whose Keystone account could not be resolved
	// without guessing, so their role was NOT assigned. These need a human — a
	// wrong guess creates an account nobody logs into while the role points
	// nowhere, which is invisible until someone reports missing access.
	PreseedConflicts []osclient.PreseedConflict `json:"preseed_conflicts,omitempty"`
	// ManagedTag is configuration rather than run state: the admin UI shows the
	// CLI query listing the managed projects, and that query has to name the tag
	// THIS deployment writes. Hardcoding it in the frontend would go quietly
	// wrong the day someone overrides the env.
	ManagedTag string `json:"managed_tag,omitempty"`
	// RecentRuns and Problems are what the recent runs logged as warnings and
	// errors (problems.go), newest first.
	RecentRuns []RunSummary `json:"recent_runs"`
	Problems   []Problem    `json:"problems"`
}

// Reconciler orchestrates the two-way sync.
type Reconciler struct {
	store    ReconcilerStore
	osClient *osclient.OpenStackClient
	cfg      Config
	// catalog is read through resources(): root admins add and withdraw
	// availabilities while the reconciler runs (package catalog).
	catalog      common.ResourceCatalog
	roleProvider common.RoleProvider
	log          *zap.SugaredLogger

	mu      sync.RWMutex
	status  Status
	trigger chan struct{}
	// followUps counts the extra passes in a row scheduled while a project is
	// being emptied (see scheduleFollowUp).
	followUps int

	// scopeParentID is the effective scope parent resolved from Config
	// (ScopeParentID, or ScopeParentName looked up / created in OpenStack).
	// Set at the start of every Reconcile run; only accessed from the
	// single-threaded reconcile loop.
	scopeParentID string
	// scopeParentSeen is the same, for the API's goroutines.
	scopeParentSeen atomic.Pointer[string]

	// usage, when set, records each finished day's consumption after a pass.
	usage UsageCollector

	// problems keeps what the recent runs logged at warning level and above.
	problems *problemRecorder
}

// UsageCollector records what projects used per day (package usage). An
// interface so this package does not depend on where the rows are kept.
type UsageCollector interface {
	CatchUp(ctx context.Context) (int, error)
}

// SetUsageCollector makes every pass also record the days not recorded yet.
// It reads from OpenStack only, so it runs in dry-run mode as well.
func (r *Reconciler) SetUsageCollector(c UsageCollector) { r.usage = c }

// When the OpenStack project of a released leaf is deleted, see Config.ReleasedDelete.
// Ordered from never to at once; each deletes in every case the one before does.
const (
	ReleasedDeleteNever       = "never"
	ReleasedDeleteOnRequest   = "on-request"
	ReleasedDeleteAfterGrace  = "after-grace"
	ReleasedDeleteImmediately = "immediately"
)

// ValidateReleasedDelete refuses a ReleasedDelete value it does not know —
// loudly at startup, rather than by quietly never deleting anything.
func ValidateReleasedDelete(v string) error {
	switch v {
	case "", ReleasedDeleteNever, ReleasedDeleteOnRequest, ReleasedDeleteAfterGrace, ReleasedDeleteImmediately:
		return nil
	}
	return fmt.Errorf("RECONCILER_RELEASED_DELETE must be %q, %q, %q or %q, got %q",
		ReleasedDeleteNever, ReleasedDeleteOnRequest, ReleasedDeleteAfterGrace, ReleasedDeleteImmediately, v)
}

// DeletesOnRequest reports whether a reconciler in this mode acts on a request
// to delete a project — every mode except never.
func DeletesOnRequest(mode string) bool {
	return mode != "" && mode != ReleasedDeleteNever
}

// New creates a Reconciler. managedProjects must match AppConfiguration.ProjectDefinitions
// and are used to drive quota translation and overcommit detection.
// roleProvider is optional (may be nil); when set it is used to resolve group memberships
// so Keystone groups can be populated during reconciliation.
func New(
	store ReconcilerStore,
	osClient *osclient.OpenStackClient,
	cfg Config,
	managedProjects []common.ManagedProject,
	roleProvider common.RoleProvider,
	log *zap.SugaredLogger,
) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.GroupPrefix == "" {
		cfg.GroupPrefix = "managed-"
	}
	problems := &problemRecorder{}
	return &Reconciler{
		store:        store,
		osClient:     osClient,
		cfg:          cfg,
		catalog:      common.StaticCatalog(managedProjects),
		roleProvider: roleProvider,
		log:          withRecorder(log, problems),
		trigger:      make(chan struct{}, 1),
		problems:     problems,
	}
}

// Start launches the background ticker and blocks until ctx is cancelled.
// Call it in a goroutine from app.go.
func (r *Reconciler) Start(ctx context.Context) {
	r.log.Infow("Reconciler started", "interval", r.cfg.Interval, "dry_run", r.cfg.DryRun)
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	// Run once immediately on startup so the state is consistent from the first request.
	r.runOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("Reconciler stopped")
			return
		case <-ticker.C:
			r.runOnce(ctx)
		case <-r.trigger:
			r.runOnce(ctx)
		}
	}
}

// Trigger requests an immediate reconciliation run. Non-blocking: if a run is already
// queued the second signal is silently dropped. A trigger during a run is kept and
// starts the next run once this one ends, so a change made mid-run is not lost:
// the queued run has not read the state yet.
//
// Every API write triggers (tree.NotifyOnWrite). That holds per process — with
// more than one replica, only the one that served the request would run.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// GetStatus returns a snapshot of the last reconciliation outcome.
func (r *Reconciler) GetStatus() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status := r.status
	status.ManagedTag = r.cfg.ManagedProjectTag
	status.RecentRuns, status.Problems = r.problems.snapshot()
	return status
}

// recordPreseedConflicts appends conflicts to the status of the current run.
// Deliberately additive within a run and cleared at its start: the list must
// describe the situation as of the latest run, not accumulate forever.
func (r *Reconciler) recordPreseedConflicts(conflicts []osclient.PreseedConflict) {
	if len(conflicts) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.PreseedConflicts = append(r.status.PreseedConflicts, conflicts...)
}

func (r *Reconciler) runOnce(ctx context.Context) {
	r.mu.Lock()
	r.status.Running = true
	r.status.PreseedConflicts = nil
	r.mu.Unlock()
	r.problems.begin(time.Now())

	result, err := r.Reconcile(ctx)
	r.scheduleFollowUp(result.purgesPending)

	// After the pass, not inside it: a day's usage is collected once and
	// does nothing on the passes after, and a failure here must not mark
	// the reconciliation itself as failed.
	if r.usage != nil {
		if n, uerr := r.usage.CatchUp(ctx); uerr != nil {
			r.log.Warnw("Usage collection failed", "error", uerr, "days_collected", n)
		}
	}

	r.mu.Lock()
	r.status.Running = false
	r.status.LastRunAt = time.Now()
	if err != nil {
		r.status.LastError = err.Error()
		r.log.Errorw("Reconciliation failed", "error", err)
	} else {
		r.status.LastError = ""
		r.status.ProjectsSynced = result.projectsSynced
		r.status.ProjectsCreated = result.projectsCreated
		r.status.ImportedLeaves = result.importedLeaves
		r.status.ImportedRemoved = result.importedRemoved
		r.status.ReleasedLeavesRemoved = result.releasedLeavesRemoved
		r.status.OrphanedUsersRemoved = result.orphanedUsersRemoved
		r.status.GroupsCreated = result.groupsCreated
		r.status.GroupsSynced = result.groupsSynced
		r.status.ProjectsTaggedForDeletion = result.projectsTaggedForDeletion
		r.status.ProjectsArchived = result.projectsArchived
		r.status.ServersShelved = result.serversShelved
		r.status.FloatingIPsReleased = result.floatingIPsReleased
		r.status.ProjectsDeleted = result.projectsDeleted
		r.status.ResourcesPurged = result.resourcesPurged
		r.status.ProjectsPromoted = result.projectsPromoted
		r.status.ProjectsRetagged = result.projectsRetagged
		r.log.Infow("Reconciliation complete",
			"synced", result.projectsSynced,
			"created", result.projectsCreated,
			"imported", result.importedLeaves,
			"imported_removed", result.importedRemoved,
			"released_removed", result.releasedLeavesRemoved,
			"orphaned_users_removed", result.orphanedUsersRemoved,
			"groups_created", result.groupsCreated,
			"groups_synced", result.groupsSynced,
			"tagged_for_deletion", result.projectsTaggedForDeletion,
			"archived", result.projectsArchived,
			"deleted", result.projectsDeleted,
			"purged", result.resourcesPurged,
			"promoted", result.projectsPromoted,
			"retagged", result.projectsRetagged)
	}
	r.mu.Unlock()
	r.problems.end(time.Now(), err)
}

type reconcileResult struct {
	projectsSynced            int
	projectsCreated           int
	importedLeaves            int
	importedRemoved           int
	releasedLeavesRemoved     int
	orphanedUsersRemoved      int
	groupsCreated             int
	groupsSynced              int
	projectsTaggedForDeletion int
	projectsArchived          int
	serversShelved            int
	floatingIPsReleased       int
	projectsDeleted           int
	resourcesPurged           int
	// purgesPending counts projects still being emptied after this pass.
	purgesPending    int
	projectsPromoted int
	projectsRetagged int
}

// listLeaves loads project leaves in the given statuses.
func (r *Reconciler) listLeaves(ctx context.Context, statuses []string) ([]tree.Node, error) {
	return r.store.ListNodes(ctx, tree.NodeQuery{
		Kinds:    []string{tree.KindProject},
		Statuses: statuses,
	}, 0, 0)
}

// Reconcile performs one full two-way sync and returns a summary.
// Safe to call directly without Start (e.g. from tests).
func (r *Reconciler) Reconcile(ctx context.Context) (reconcileResult, error) {
	var res reconcileResult

	// ── Phase 1: load state from both sides ──────────────────────────────────

	activeLeaves, err := r.listLeaves(ctx, tree.ReconcilableStatuses)
	if err != nil {
		return res, fmt.Errorf("load active leaves: %w", err)
	}

	// allKnownLeaves covers every real status so Phase 5 can tell whether a tagged
	// OS project is already tracked (in any state) before deciding to import it.
	// This prevents projects from being re-imported when their leaf is e.g.
	// pending, rejected, or released.
	allKnownLeaves, err := r.listLeaves(ctx, tree.KnownStatuses)
	if err != nil {
		return res, fmt.Errorf("load all known leaves: %w", err)
	}

	// releasedLeafByID is a subset of allKnownLeaves used in Phase 5 to tag,
	// archive or delete OS projects whose leaf has been released (or archived
	// since).
	releasedLeafByID := make(map[string]tree.Node, len(allKnownLeaves))
	for _, leaf := range allKnownLeaves {
		if tree.IsRetiredStatus(leaf.Status) {
			releasedLeafByID[leaf.ID] = leaf
		}
	}

	existingImported, err := r.listLeaves(ctx, []string{tree.StatusImported})
	if err != nil {
		return res, fmt.Errorf("load imported leaves: %w", err)
	}

	// Every budget, so a project's description can name its path in the tree.
	budgets, err := r.store.ListNodes(ctx, tree.NodeQuery{Kinds: []string{tree.KindBudget}}, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load budgets: %w", err)
	}
	budgetByID := make(map[string]tree.Node, len(budgets))
	for _, b := range budgets {
		budgetByID[b.ID] = b
	}

	scopeParentID, err := r.ensureScopeParent()
	if err != nil {
		return res, fmt.Errorf("resolve scope parent: %w", err)
	}

	// Under the modern RBAC defaults Nova/Neutron/Cinder only accept quota and
	// grant calls from a PROJECT-scoped admin token, while the primary client
	// may be domain-scoped (that is what project creation needs). The client
	// can build a second, project-scoped provider for exactly those services —
	// scoped to the scope parent, the one project that outlives all managed
	// ones. A no-op for every other auth method, and a failure only means
	// quota calls keep being refused (and retried), which the sync path
	// already reports loudly.
	if scopeParentID != "" {
		if err := r.osClient.EnsureProjectScope(scopeParentID); err != nil {
			r.log.Warnw("Project-scoped service clients unavailable; quota and grant calls may be refused",
				"scope_project_id", scopeParentID, "error", err)
		}
	}

	osProjects, err := r.loadScopedOSProjects(scopeParentID)
	if err != nil {
		return res, fmt.Errorf("list OS projects: %w", err)
	}

	// ── Phase 2: build lookup maps ────────────────────────────────────────────

	leafByID := make(map[string]tree.Node, len(activeLeaves))
	for _, leaf := range activeLeaves {
		leafByID[leaf.ID] = leaf
	}

	// knownLeafIDs covers all real statuses; used in Phase 5 to avoid re-importing
	// a tagged OS project whose leaf exists but is not in a reconcilable state.
	knownLeafIDs := make(map[string]struct{}, len(allKnownLeaves))
	for _, leaf := range allKnownLeaves {
		knownLeafIDs[leaf.ID] = struct{}{}
	}

	// osProjectByResourceID: tagged OS projects keyed by their embedded leaf ID.
	osProjectByResourceID := make(map[string]osclient.ProjectInfo, len(osProjects))
	// osProjectByOSID: all scoped OS projects keyed by their OS project ID.
	osProjectByOSID := make(map[string]osclient.ProjectInfo, len(osProjects))
	for _, p := range osProjects {
		osProjectByOSID[p.ID] = p
		if rid := r.osClient.ExtractResourceIDFromTags(p.Tags); rid != "" {
			osProjectByResourceID[rid] = p
		}
	}

	// importedByOSProjectID: existing imported leaves keyed by their OSProjectID.
	importedByOSProjectID := make(map[string]tree.Node, len(existingImported))
	for _, leaf := range existingImported {
		if leaf.OSProjectID != "" {
			importedByOSProjectID[leaf.OSProjectID] = leaf
		}
	}

	// ── Phase 2.5: Promote imported leaves flagged for promotion ─────────────
	//
	// Must run after the lookup maps are built (Phase 2) but before Phase 3/4 so
	// that the newly-tagged OS project is not re-imported in Phase 5.
	// Promoted entries are removed from osProjectByOSID and importedByOSProjectID
	// so the Phase 5 loops never see them.
	r.promoteImportedLeaves(ctx, existingImported, osProjectByOSID, importedByOSProjectID, osProjectByResourceID, &res)

	// ── Phase 3: Sync OS groups and their memberships ───────────────────────
	// Collect every group: token referenced across all active leaves, then
	// ensure each maps to a real Keystone group and its members are up to date.

	// groupTokenToOSID maps a group token (e.g. "group:dept_cs_faculty") to the
	// corresponding Keystone group ID. Built here and reused in Phase 4 when
	// assigning groups to projects.
	groupTokenToOSID := r.syncGroups(ctx, activeLeaves, &res)

	// ── Phase 4: Storage → OpenStack (project create / quota sync) ───────────

	// claimedOSProjects guards against two leaves recovering the same project:
	// the first one keeps it, the second is treated as having none.
	claimedOSProjects := make(map[string]string, len(activeLeaves))

	for _, leaf := range activeLeaves {
		description := buildDescription(leaf, budgetPath(budgetByID, leaf))
		osProject, hasProject := osProjectByResourceID[leaf.ID]
		if !hasProject {
			// The tag is the only thing tying a project to its node, and it sits
			// in OpenStack where a project admin can remove it. Before concluding
			// the project is gone — and building a second one beside the first —
			// look it up by the ID stored when it was created, and put the tag back.
			if recovered, ok := r.recoverUntaggedProject(ctx, leaf,
				osProjectByOSID, importedByOSProjectID, claimedOSProjects, &res); ok {
				osProjectByResourceID[leaf.ID] = recovered
				osProject, hasProject = recovered, true
			}
		}
		if !hasProject {
			created, err := r.createOpenstackProjectForLeaf(ctx, leaf, description)
			if err != nil {
				r.log.Warnw("Failed to create OS project for leaf", "node_id", leaf.ID, "error", err)
				continue
			}
			res.projectsCreated++
			leaf.OSProjectID = created.ID
			if !r.cfg.DryRun {
				r.persistOSProjectID(ctx, leaf.ID, created.ID)
			}
			r.syncMembers(leaf, created.ID)
			r.syncGroupAssignments(leaf, created.ID, groupTokenToOSID)
		} else {
			m, err := r.syncQuota(leaf, osProject, description)
			if err != nil {
				r.log.Warnw("Failed to sync quota for leaf", "node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
			if applyOSSyncState(&leaf, osProject.ID, m) && !r.cfg.DryRun {
				r.persistOSSyncState(ctx, leaf.ID, osProject.ID, m)
			}
			r.syncMembers(leaf, osProject.ID)
			r.syncGroupAssignments(leaf, osProject.ID, groupTokenToOSID)
			r.syncGrants(leaf, osProject.ID)
			res.projectsSynced++
		}
	}

	// ── Phase 5: OpenStack → Storage (import / remove imported leaves) ───────

	for osID, osProject := range osProjectByOSID {
		resourceID := r.osClient.ExtractResourceIDFromTags(osProject.Tags)
		if resourceID != "" {
			if _, active := leafByID[resourceID]; active {
				continue // managed + active → handled in phase 4
			}
			if releasedLeaf, wasReleased := releasedLeafByID[resourceID]; wasReleased {
				// Leaf was released: tag, archive or delete the project.
				r.handleReleasedProject(ctx, osProject, releasedLeaf, scopeParentID, &res)
				// Seen in OpenStack, so it is not a candidate for removal below.
				delete(releasedLeafByID, resourceID)
				delete(importedByOSProjectID, osID)
				continue
			}
			if _, known := knownLeafIDs[resourceID]; known {
				continue // leaf exists in a non-reconcilable state (pending/rejected/…); skip
			}
			// Resource ID points to a leaf that no longer exists in storage at all
			// (e.g. hard-deleted) — treat as orphaned and import.
		}
		// Either untagged (externally created) or orphaned — import as an imported leaf.
		r.upsertImported(ctx, osProject, importedByOSProjectID, &res)
		delete(importedByOSProjectID, osID) // mark as seen so we don't remove it below
	}

	// Clean up imported leaves whose OS projects are no longer in scope.
	r.removeStaleImports(ctx, importedByOSProjectID, osProjectByOSID, &res)

	// Released leaves whose OpenStack project has been deleted are removed too.
	//
	// A released leaf costs nothing (usage sums approved and change_pending only)
	// and stays as the record of what someone had. Responsibility for the actual
	// deletion is handed to OpenStack via the pending-deletion and contact tags,
	// so the honest end of that record is the moment the project is really gone.
	r.removeReleasedLeavesWithoutProject(ctx, releasedLeafByID, osProjectByOSID, &res)

	// ── Phase 6: Remove auto-created Keystone users with no project memberships ─
	//
	// Users are pre-created by FindOrCreateUser when a leaf is approved. Once
	// a leaf is released/rejected and all project memberships are removed, the
	// Keystone account becomes an orphan. We delete it here so the identity
	// service stays clean over time.
	//
	// Safety invariants:
	//   1. Only users whose description matches ManagedUserDescription are candidates.
	//   2. A user with ANY project role assignment (even one added manually outside
	//      this system) is never deleted.
	// Off unless Config.DeleteOrphanedUsers — invariant 2 only holds when the
	// service user sees every assignment, see pruneOrphanedUsers.
	if r.cfg.DeleteOrphanedUsers {
		r.pruneOrphanedUsers(&res)
	}

	return res, nil
}

// tagReader is the narrow OpenStack surface chooseRecoverableProject needs.
// *osclient.OpenStackClient satisfies it; tests substitute a fake.
type tagReader interface {
	ExtractResourceIDFromTags(tags []string) string
}

// removeReleasedLeavesWithoutProject drops the released leaves whose OpenStack
// project is no longer in the reconciler's scope.
//
// Out of scope counts as gone, deliberately: the scope is what this reconciler
// is responsible for, and a project moved out of it is somebody else's now. The
// alternative — asking Keystone about every unseen project to tell "deleted"
// from "moved" — buys a distinction that changes nothing we would do about it.
//
// The ones Phase 5 saw were removed from `released` as it went, so everything
// arriving here was absent from the listing. That the listing is complete is
// guaranteed upstream: a failed project listing aborts the whole run before
// this point, so "not in the list" can never mean "the list did not load".
func (r *Reconciler) removeReleasedLeavesWithoutProject(
	ctx context.Context,
	released map[string]tree.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	res *reconcileResult,
) {
	// Sorted, so a run is reproducible and its log reads the same twice.
	ids := make([]string, 0, len(released))
	for id := range released {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	for _, id := range ids {
		leaf := released[id]

		// In scope under its stored ID, just not matched by tag above: the
		// project is right there, its resource-id tag is not — which any
		// project admin can arrange in Horizon with two clicks. In scope is in
		// scope, so this is not a deletion. One map lookup, no API call.
		if _, inScope := osProjectByOSID[leaf.OSProjectID]; inScope && leaf.OSProjectID != "" {
			continue
		}

		// Nothing is destroyed in OpenStack here — the project is already gone
		// from it, and keeping the record would leave the tree disagreeing with
		// the cloud.
		r.log.Infow("Removing released leaf — its OpenStack project is gone",
			"node_id", leaf.ID, "name", leaf.Name, "os_project_id", leaf.OSProjectID,
			"dry_run", r.cfg.DryRun)
		if !r.cfg.DryRun {
			deleted, err := r.store.DeleteNodeIf(ctx, leaf.ID, func(n tree.Node) bool { return tree.IsRetiredStatus(n.Status) })
			if err != nil {
				r.log.Warnw("Failed to remove released leaf",
					"node_id", leaf.ID, "error", err)
				continue
			}
			if !deleted {
				continue
			}
		}
		res.releasedLeavesRemoved++
	}
}

// chooseRecoverableProject decides whether a leaf may reclaim the OpenStack
// project recorded on it. Separated from the writing half so the three ways this
// can go wrong are testable without a cloud.
func chooseRecoverableProject(
	c tagReader,
	leaf tree.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	claimed map[string]string,
	log *zap.SugaredLogger,
) (osclient.ProjectInfo, bool) {
	if leaf.OSProjectID == "" {
		return osclient.ProjectInfo{}, false // never had one — Phase 4 creates it
	}
	osProject, exists := osProjectByOSID[leaf.OSProjectID]
	if !exists {
		return osclient.ProjectInfo{}, false // really gone (deleted, or moved out of scope)
	}

	// Tagged for somebody else: the ID on this leaf is stale, and taking the
	// project back would take it away from the node that owns it now.
	if taggedFor := c.ExtractResourceIDFromTags(osProject.Tags); taggedFor != "" && taggedFor != leaf.ID {
		log.Warnw("Stored OS project is tagged for a different node — not reclaiming it",
			"node_id", leaf.ID, "os_project_id", osProject.ID, "tagged_node_id", taggedFor)
		return osclient.ProjectInfo{}, false
	}
	if owner, taken := claimed[osProject.ID]; taken {
		log.Warnw("Two leaves point at the same OS project — leaving the second one without",
			"node_id", leaf.ID, "os_project_id", osProject.ID, "claimed_by", owner)
		return osclient.ProjectInfo{}, false
	}
	return osProject, true
}

// recoverUntaggedProject finds a leaf's OpenStack project when the resource-id
// tag that normally identifies it is gone, and restores the tag.
//
// Matching runs on that tag alone, and the tag lives in OpenStack: a project
// admin — which every project owner is — can drop it in Horizon with two clicks.
// Without this, the next tick sees a leaf with no project and builds a SECOND
// one beside the first, while the original reappears in "Unassigned" as an
// import, VMs and all. The node's stored os_project_id is the second witness
// that survives whatever happens to the tags, so it decides.
//
// The project is claimed even when re-tagging fails: a duplicate project is far
// worse than a tag that is restored one tick later, and every following tick
// retries. Returns the project and whether the leaf may use it.
func (r *Reconciler) recoverUntaggedProject(
	ctx context.Context,
	leaf tree.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	importedByOSProjectID map[string]tree.Node,
	claimed map[string]string,
	res *reconcileResult,
) (osclient.ProjectInfo, bool) {
	osProject, ok := chooseRecoverableProject(r.osClient, leaf, osProjectByOSID, claimed, r.log)
	if !ok {
		return osclient.ProjectInfo{}, false
	}

	r.log.Warnw("OS project lost its resource-id tag — restoring it instead of creating a second project",
		"node_id", leaf.ID, "os_project_id", osProject.ID,
		"project_name", osProject.Name, "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if err := r.osClient.TagProjectForNode(osProject.ID, leaf.ID, osProject.Tags); err != nil {
			r.log.Warnw("Failed to restore the resource-id tag — keeping the project anyway, retrying next tick",
				"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
		} else {
			osProject.Tags = append(slices.Clone(osProject.Tags), r.osClient.ResourceIDTag(leaf.ID))
		}
	}

	claimed[osProject.ID] = leaf.ID
	res.projectsRetagged++

	// Keep Phase 5 away from it: its copy of the tags is from before the repair,
	// so it would import the project a second time as an unmanaged one.
	delete(osProjectByOSID, osProject.ID)

	// An earlier tick may already have imported it that way. That import and this
	// leaf are the same OpenStack project, and showing it twice — once as someone's
	// project, once as an import waiting to be adopted — is how a manager ends up
	// adopting a project that is already managed.
	if shadow, imported := importedByOSProjectID[osProject.ID]; imported {
		delete(importedByOSProjectID, osProject.ID)
		r.log.Infow("Removing the imported leaf that shadowed a managed project",
			"node_id", leaf.ID, "shadow_node_id", shadow.ID, "os_project_id", osProject.ID)
		if !r.cfg.DryRun {
			deleted, err := r.store.DeleteNodeIf(ctx, shadow.ID, isStillImported)
			if err != nil {
				r.log.Warnw("Failed to delete the shadowing imported leaf",
					"shadow_node_id", shadow.ID, "error", err)
			}
			if err != nil || !deleted {
				return osProject, true
			}
		}
		res.importedRemoved++
	}

	return osProject, true
}

// removeFlag returns a new slice with all occurrences of flag removed.
func removeFlag(flags []string, flag string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if f != flag {
			out = append(out, f)
		}
	}
	return out
}

// promoteImportedLeaves processes imported leaves that carry the
// FlagPromoteOnReconcile flag (set by the promote API together with the new
// parent and owner). For each:
//  1. The OS project is tagged with the managed marker and the leaf's node ID.
//  2. The leaf's status is changed to "pending" and the flag is removed.
//  3. The entry is removed from the Phase-5 lookup maps so it is not re-imported.
//
// After this phase the leaf flows through the normal pending → approved cycle
// under its new parent budget. Non-fatal: failures are logged and skipped.
func (r *Reconciler) promoteImportedLeaves(
	ctx context.Context,
	existingImported []tree.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	importedByOSProjectID map[string]tree.Node,
	osProjectByResourceID map[string]osclient.ProjectInfo,
	res *reconcileResult,
) {
	for _, leaf := range existingImported {
		if !slices.Contains(leaf.Flags, tree.FlagPromoteOnReconcile) {
			continue
		}

		osProject, ok := osProjectByOSID[leaf.OSProjectID]
		if !ok {
			r.log.Warnw("Cannot promote: OS project not found in scope",
				"node_id", leaf.ID, "os_project_id", leaf.OSProjectID)
			continue
		}

		r.log.Infow("Promoting imported leaf to managed project",
			"node_id", leaf.ID, "os_project_id", leaf.OSProjectID, "dry_run", r.cfg.DryRun)

		if !r.cfg.DryRun {
			if err := r.osClient.TagProjectForNode(osProject.ID, leaf.ID, osProject.Tags); err != nil {
				r.log.Warnw("Failed to tag OS project for promotion",
					"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
		}

		if !r.cfg.DryRun {
			// The members as OpenStack has them NOW. The leaf only knows them as
			// of the last pass before the promotion was asked for, and once the
			// project is approved the member sync removes everyone the leaf does
			// not name — so a member added in between would lose access. Without
			// this list the promotion waits for the next pass.
			members, err := r.osClient.ListProjectMemberInfo(osProject.ID)
			if err != nil {
				r.log.Warnw("Cannot promote yet: the project's members could not be read",
					"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
			current := importMembers(members)
			promoted, err := r.store.UpdateNode(ctx, leaf.ID, func(n *tree.Node) error {
				// The promotion may have been withdrawn while this pass talked to
				// OpenStack; then there is nothing to promote any more.
				if n.Status != tree.StatusImported || !slices.Contains(n.Flags, tree.FlagPromoteOnReconcile) {
					return tree.ErrSkipUpdate
				}
				n.AuthorizedUsers = mergeMembers(n.AuthorizedUsers, current, n.Owner)
				n.Status = tree.StatusPending
				n.Flags = removeFlag(n.Flags, tree.FlagPromoteOnReconcile)
				return nil
			})
			if err != nil {
				r.log.Warnw("Failed to persist promoted leaf",
					"node_id", leaf.ID, "error", err)
				continue
			}
			if !promoted {
				continue
			}
		}

		// Remove from Phase-5 maps so the newly-tagged OS project is not re-imported.
		delete(osProjectByOSID, leaf.OSProjectID)
		delete(importedByOSProjectID, leaf.OSProjectID)
		osProjectByResourceID[leaf.ID] = osProject

		res.projectsPromoted++
	}
}

// pruneOrphanedUsers finds auto-created Keystone users that have no project role
// assignments and deletes them. Non-fatal: errors for individual users are logged
// but do not abort the reconciliation run.
// pruneOrphanedUsers deletes the accounts this service created that hold no
// project role any more.
//
// Switched off by default (Config.DeleteOrphanedUsers), because "no role" is
// only what the service user can SEE. With admin limited to its own domain —
// the setup on both clouds, where svc-os-mgt is admin on dhbw-managed only —
// Keystone hides the role assignments on projects in other domains. An
// account that still has a role elsewhere, granted by someone else, then
// looks orphaned and is deleted, and the person loses that access too. It
// happened on staging (2026-10-07): an account whose only role was on a
// project in the default domain was deleted as soon as the service user
// switched to domain-limited admin.
//
// Keeping such an account costs nothing: without a role it reaches nothing,
// and the next SSO login would bring it back anyway. Turn it on only where
// the service user sees every assignment (system or cloud-wide admin).
func (r *Reconciler) pruneOrphanedUsers(res *reconcileResult) {
	if r.osClient == nil {
		return
	}

	orphans, err := r.osClient.CollectOrphanedManagedUsers()
	if err != nil {
		r.log.Warnw("Could not collect orphaned managed users, skipping cleanup", "error", err)
		return
	}

	for _, u := range orphans {
		r.log.Infow("Deleting orphaned managed user (no project memberships)",
			"user_id", u.ID, "name", u.Name, "dry_run", r.cfg.DryRun)
		if r.cfg.DryRun {
			res.orphanedUsersRemoved++
			continue
		}
		if err := r.osClient.DeleteUser(u.ID); err != nil {
			r.log.Warnw("Failed to delete orphaned managed user",
				"user_id", u.ID, "name", u.Name, "error", err)
			continue
		}
		res.orphanedUsersRemoved++
	}
}

// handleReleasedProject takes care of the OpenStack project of a released or
// archived leaf, on every pass, as configured:
//
//   - always: it is tagged with the day it is due for deletion
//     (<PendingDeletionTagPrefix><YYYY-MM-DD>, release day plus the grace
//     period), its owner (<ContactTagPrefix><email>) and its status;
//   - when ReleasedDelete says it is due (see deletionDue): it is emptied and
//     deleted over the next passes, and then the leaf — see purgeRetiredProject;
//   - otherwise, with ReleasedArchive: it is archived (see
//     archiveReleasedProject), and the leaf moves to archived once that is
//     complete.
//
// Each step looks at the project first and does only what is still missing,
// so a pass that failed halfway is finished by the next one.
func (r *Reconciler) handleReleasedProject(ctx context.Context, osProject osclient.ProjectInfo, leaf tree.Node, scopeParentID string, res *reconcileResult) {
	// Archiving rewrites the tags too, so it starts from what tagging left.
	osProject.Tags = r.tagReleasedProject(osProject, leaf, res)
	if deletionDue(r.cfg.ReleasedDelete, leaf, osProject.Tags, r.cfg.PendingDeletionTagPrefix, time.Now()) {
		r.purgeRetiredProject(ctx, osProject, leaf, scopeParentID, res)
		return
	}
	archived := r.isArchived(osProject.Tags)
	if r.cfg.ReleasedArchive && !archived {
		archived = r.archiveReleasedProject(osProject, leaf, res)
	}
	if archived && leaf.Status == tree.StatusReleased {
		r.markLeafArchived(ctx, leaf)
	}
}

// isArchived reports whether the project carries the archive mark.
func (r *Reconciler) isArchived(tags []string) bool {
	return slices.ContainsFunc(tags, func(t string) bool { return strings.HasPrefix(t, r.archivedTagPrefix()) })
}

func (r *Reconciler) archivedTagPrefix() string {
	if r.cfg.ArchivedTagPrefix == "" {
		return "archived:"
	}
	return r.cfg.ArchivedTagPrefix
}

// markLeafArchived records in the tree what the project already shows: it is
// archived. From then on the leaf costs what Accounting.ChargeArchived says.
func (r *Reconciler) markLeafArchived(ctx context.Context, leaf tree.Node) {
	if r.cfg.DryRun {
		r.log.Infow("Dry run: would mark leaf archived", "node_id", leaf.ID)
		return
	}
	if _, err := r.store.UpdateNode(ctx, leaf.ID, func(n *tree.Node) error {
		if !tree.MarkArchived(n) {
			return tree.ErrSkipUpdate
		}
		return nil
	}); err != nil {
		r.log.Warnw("Failed to mark leaf archived", "node_id", leaf.ID, "error", err)
	}
}

// tagReleasedProject writes the pending-deletion, contact and status tags.
// Idempotent: the deletion day is set once and never moved — re-dating it on
// every pass would push it out for ever.
func (r *Reconciler) tagReleasedProject(osProject osclient.ProjectInfo, leaf tree.Node, res *reconcileResult) []string {
	tagged := false
	for _, tag := range osProject.Tags {
		if strings.HasPrefix(tag, r.cfg.PendingDeletionTagPrefix) {
			tagged = true
		}
	}

	graceDays := r.cfg.ReleasedDeleteGraceDays
	if graceDays <= 0 {
		graceDays = 30
	}
	deletionDate := time.Now().AddDate(0, 0, graceDays).Format("2006-01-02")
	pending := ""
	if !tagged {
		pending = deletionDate
	}
	newTags, changed := applyPrefixedTags(osProject.Tags,
		prefixedTag{r.cfg.ContactTagPrefix, leaf.OwnerEmail()},
		// The status tag rides along in this same write. A released leaf never
		// reaches the normal sync path — that one only runs for the reconcilable
		// statuses — so without this the project would keep the "approved" it
		// was last tagged with, which is the one moment a workflow must not be
		// misled.
		prefixedTag{r.cfg.StatusTagPrefix, leaf.Status},
	)
	if pending != "" {
		newTags = append(newTags, r.cfg.PendingDeletionTagPrefix+pending)
		changed = true
	}
	if !changed {
		return osProject.Tags
	}

	r.log.Infow("Tagging OS project for pending deletion",
		"os_project_id", osProject.ID, "node_id", leaf.ID,
		"deletion_date", deletionDate, "already_tagged", tagged,
		"status", leaf.Status, "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if _, err := r.osClient.UpdateProject(osProject.ID, osclient.ProjectUpdateOpts{
			Tags: &newTags,
		}); err != nil {
			r.log.Warnw("Failed to tag OS project for pending deletion",
				"os_project_id", osProject.ID, "node_id", leaf.ID, "error", err)
			return osProject.Tags
		}
	}
	res.projectsTaggedForDeletion++
	return newTags
}

// syncTerminationTag publishes a leaf's termination date on its OpenStack project
// as <TerminationTagPrefix><RFC3339>, so "what runs out when" can be answered from
// OpenStack alone — `openstack project list` and the Keystone API both return tags,
// and an operator holding neither an account here nor database access can read the
// date off the project itself. The value is the stored timestamp verbatim rather
// than a truncated date: it is what the API would answer, and a tag is a bad place
// to lose precision.
//
// Publishing only. Nothing enforces the date (see Node.TerminationDate) — a project
// past it keeps running, it is now merely visible.
//
// The owner rides along as <ContactTagPrefix><email>: whoever finds a project
// about to run out in OpenStack needs someone to write to, and that question
// used to be answered only once a project was released. Only the owner — the
// one person responsible — and kept current, so a transfer moves the tag too.
//
// Writes only on an actual change, because this runs for every managed leaf on every
// tick: an unconditional update would be one Keystone write per project per interval
// for a value that changes maybe twice in a project's life. Clearing the date in the
// tree removes the tag on the next tick, and every other tag (managed, resource-id,
// pending-deletion) is carried over untouched.
func (r *Reconciler) syncManagedTags(leaf tree.Node, osProject osclient.ProjectInfo) {
	termination := ""
	if leaf.TerminationDate != nil {
		termination = *leaf.TerminationDate
	}

	// One rebuild and at most one write for both tags. Two independent syncs
	// would each rebuild from this same snapshot, so whichever wrote second
	// would carry the other's old value back — an update that silently undoes
	// the one before it, on exactly the ticks where both changed.
	newTags, changed := applyPrefixedTags(osProject.Tags,
		prefixedTag{r.cfg.TerminationTagPrefix, termination},
		prefixedTag{r.cfg.StatusTagPrefix, leaf.Status},
		prefixedTag{r.cfg.ContactTagPrefix, leaf.OwnerEmail()},
	)
	if !changed {
		return
	}

	r.log.Infow("Updating tags on OS project",
		"node_id", leaf.ID, "os_project_id", osProject.ID,
		"status", leaf.Status, "termination", termination, "contact", leaf.OwnerEmail(),
		"tags", newTags, "dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return
	}
	if _, err := r.osClient.UpdateProject(osProject.ID, osclient.ProjectUpdateOpts{
		Tags: &newTags,
	}); err != nil {
		r.log.Warnw("Failed to update tags on OS project",
			"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
	}
}

// prefixedTag is one "<prefix><value>" tag the reconciler owns: it replaces any
// existing tag with that prefix, and an empty value removes it.
type prefixedTag struct {
	prefix string
	value  string
}

// applyPrefixedTags brings every owned prefix to its desired value in one pass and
// reports whether anything actually changed. An empty prefix is skipped, which is
// how a tag is switched off by configuration.
//
// The caller writes only when changed is true, because this runs for every managed
// leaf on every tick: an unconditional update would be one Keystone write per
// project per interval for values that change a handful of times in a project's
// life. Tags outside the owned prefixes (managed, resource-id, pending-deletion)
// are carried over untouched.
//
// "Changed" is decided per prefix, NOT by comparing the two lists: the owned tags
// are re-appended at the end, so a positional comparison would report a change on
// the first pass and then again on every pass if Keystone ever hands the list back
// in a different order — a write per project per tick, forever, with no difference
// to show for it.
func applyPrefixedTags(tags []string, owned ...prefixedTag) ([]string, bool) {
	changed := false
	out := make([]string, 0, len(tags)+len(owned))

	for _, tag := range tags {
		kept := true
		for _, o := range owned {
			if o.prefix != "" && strings.HasPrefix(tag, o.prefix) {
				kept = false
				if tag != o.prefix+o.value {
					changed = true // replaced, or removed because value is empty
				}
				break
			}
		}
		if kept {
			out = append(out, tag)
		}
	}

	for _, o := range owned {
		if o.prefix == "" || o.value == "" {
			continue
		}
		desired := o.prefix + o.value
		out = append(out, desired)
		if !slices.Contains(tags, desired) {
			changed = true // newly added
		}
	}
	return out, changed
}

// loadScopedOSProjects fetches the OS projects to reconcile against.
// When a scope parent is configured it lists all children of that parent so externally
// created projects can be imported. Otherwise only managed-tagged projects.
func (r *Reconciler) loadScopedOSProjects(scopeParentID string) ([]osclient.ProjectInfo, error) {
	if scopeParentID != "" {
		return r.osClient.CollectProjectsByParent(scopeParentID)
	}
	return r.osClient.CollectManagedProjects()
}

// scopeParentClient is the narrow OpenStack surface needed to resolve the scope parent.
// *osclient.OpenStackClient satisfies it; tests substitute a fake.
type scopeParentClient interface {
	FindProjectByName(name string) (*projects.Project, error)
	CreateProject(opts osclient.ProjectCreateOpts) (*projects.Project, error)
}

// ScopeParentDescription is written to the scope parent project when the reconciler
// creates it from ScopeParentName.
const ScopeParentDescription = "Parent project for all projects managed by the OpenStack management API. Do not modify manually."

// resolveScopeParent returns the OS project ID to scope reconciliation to.
// Precedence: explicit ScopeParentID > ScopeParentName (looked up by name, created on
// demand) > "" (unscoped, tag-based discovery only). In dry-run mode a missing named
// parent is NOT created and the run proceeds unscoped.
func resolveScopeParent(c scopeParentClient, cfg Config, log *zap.SugaredLogger) (string, error) {
	if cfg.ScopeParentID != "" {
		return cfg.ScopeParentID, nil
	}
	if cfg.ScopeParentName == "" {
		return "", nil
	}
	project, err := c.FindProjectByName(cfg.ScopeParentName)
	if err != nil {
		return "", fmt.Errorf("look up scope parent %q: %w", cfg.ScopeParentName, err)
	}
	if project != nil {
		return project.ID, nil
	}
	if cfg.DryRun {
		log.Infow("Would create scope parent project; proceeding unscoped",
			"name", cfg.ScopeParentName, "dry_run", true)
		return "", nil
	}
	// Deliberately no managed/resource-id tags: the scope parent must never look like
	// a managed leaf, or the import and deletion phases could touch it.
	description := ScopeParentDescription
	enabled := true
	created, err := c.CreateProject(osclient.ProjectCreateOpts{
		BaseProjectOpts: osclient.BaseProjectOpts{
			Name:        cfg.ScopeParentName,
			Description: &description,
			Enabled:     &enabled,
		},
	})
	if err != nil {
		// A concurrent creator may have won the race (409): the name is unique per
		// domain, so a successful re-lookup is authoritative.
		if again, ferr := c.FindProjectByName(cfg.ScopeParentName); ferr == nil && again != nil {
			return again.ID, nil
		}
		return "", fmt.Errorf("create scope parent %q: %w", cfg.ScopeParentName, err)
	}
	log.Infow("Created scope parent project",
		"name", cfg.ScopeParentName, "os_project_id", created.ID)
	return created.ID, nil
}

// ensureScopeParent resolves the effective scope parent and caches a successful,
// non-empty result for subsequent runs (the empty dry-run outcome is re-resolved
// every run so the project is picked up once it exists).
func (r *Reconciler) ensureScopeParent() (string, error) {
	if r.scopeParentID != "" {
		return r.scopeParentID, nil
	}
	id, err := resolveScopeParent(r.osClient, r.cfg, r.log)
	if err != nil {
		return "", err
	}
	r.scopeParentID = id
	r.scopeParentSeen.Store(&id)
	return id, nil
}

// UseCatalog replaces the catalogue given to New with one that changes at
// runtime. Called before Start.
func (r *Reconciler) UseCatalog(c common.ResourceCatalog) { r.catalog = c }

func (r *Reconciler) resources() []common.ManagedProject { return r.catalog.Resources() }

// CheckGrantTarget asks OpenStack whether g can be granted per project (for
// root admins adding an availability).
func (r *Reconciler) CheckGrantTarget(g common.Grant) error {
	parent := ""
	if p := r.scopeParentSeen.Load(); p != nil {
		parent = *p
	}
	return r.osClient.CheckGrantTarget(g, parent)
}

// GrantedProjects lists the projects OpenStack grants g to.
func (r *Reconciler) GrantedProjects(g common.Grant) ([]string, error) {
	return r.osClient.GrantedProjects(g)
}

// keystoneProjectNameMaxLen is Keystone's hard limit for project names: the API
// schema caps "name" at 64 characters and rejects anything longer with 400.
const keystoneProjectNameMaxLen = 64

// managedDescriptionSuffix is appended to every managed project's description so a
// project is recognisable as ours in Horizon, where tags are not shown. It carries
// no meaning for the reconciler — identification runs on tags alone.
const managedDescriptionSuffix = " (managed project)"

// buildProjectName constructs the OS project name for a leaf: what the leaf is
// called, whose it is, and a short form of its ID, e.g.
// "Cloud Computing @ max.muster [p_7ad31c]".
//
// "What it is called" falls back to the purpose for leaves created before the
// name became mandatory — they carry no name at all. Without the fallback such a
// project ends up named after its bare node ID ("p_7ad31c42-21e7-…"), which is
// what a user sees in Horizon and Skyline and cannot tell apart from any other.
//
// "Whose it is" is the part of the owner's address before the @. Sharing makes
// equal names common — somebody who administers or uses other people's projects
// sees every "test" there is — and the owner is what tells them apart for a
// person. The whole address would not fit: a student address alone takes half
// of Keystone's 64 characters.
//
// Name and owner get the same share of what the ID leaves: whichever needs less
// hands the rest to the other, and only what exceeds its share is cut.
//
// The ID suffix is not decoration. Keystone enforces project-name uniqueness per
// *domain*, not per parent — two leaves named "Cloud Computing" under different
// budgets, or a name already taken by a foreign project elsewhere in the domain,
// would otherwise collide with a 409. The suffix makes the name collision-free by
// construction, so no retry-with-fallback logic is needed.
//
// Nothing parses the name back: a project is matched to its node via the
// resource-id tag, so renaming a node is safe at any time.
func buildProjectName(leaf tree.Node) string {
	id := sanitizeProjectName(shortNodeID(leaf.ID))
	name := sanitizeProjectName(cmp.Or(leaf.Name, leaf.Reason))
	owner, _, _ := strings.Cut(leaf.OwnerEmail(), "@")
	owner = sanitizeProjectName(owner)

	switch {
	case id == "" && name == "":
		return "unnamed project" // defensive: a node always has an ID
	case id == "":
		return truncateRunes(name, keystoneProjectNameMaxLen)
	case name == "":
		return truncateRunes(id, keystoneProjectNameMaxLen)
	}

	suffix := " [" + id + "]"
	room := keystoneProjectNameMaxLen - utf8.RuneCountInString(suffix)
	if owner != "" {
		room -= utf8.RuneCountInString(ownerSeparator)
	}
	if room < 2 {
		// Pathologically long ID: drop the name, keep the identifying part.
		return truncateRunes(id, keystoneProjectNameMaxLen)
	}
	if owner == "" {
		return truncateRunes(name, room) + suffix
	}
	nameRoom, ownerRoom := shareRoom(utf8.RuneCountInString(name), utf8.RuneCountInString(owner), room)
	return truncateRunes(name, nameRoom) + ownerSeparator + truncateRunes(owner, ownerRoom) + suffix
}

// ownerSeparator stands between a project's name and its owner in the OS name.
const ownerSeparator = " @ "

// shareRoom splits room between two parts of length a and b: each is guaranteed
// half, and a part that needs less than its half leaves the rest to the other.
// Two parts that both need more get exactly the same length.
func shareRoom(a, b, room int) (int, int) {
	half := room / 2
	switch {
	case a+b <= room:
		return a, b
	case a <= half:
		return a, room - a
	case b <= half:
		return room - b, b
	}
	return half, half
}

// shortNodeID shortens a node ID for use inside a project name. Node IDs are a
// kind prefix plus a UUID ("p_7ad31c42-21e7-4fbd-aa3e-15a4660449be"), and 36
// characters of that are noise beside a six-letter purpose — the name is read by
// people, in Horizon and Skyline, next to dozens of others. Only the first six
// hex digits are kept, the same trade git makes with short commit hashes.
//
// Uniqueness survives it: the suffix only has to separate projects that share a
// NAME and an OWNER, and two of those collide only if their IDs also agree in
// the first six hex digits — one in sixteen million. The kind prefix stays so
// the suffix still reads as a node ID and can be pasted into a search.
//
// IDs that are not "<kind>_<uuid>" — the structural "root" and "unassigned"
// nodes — are left alone.
func shortNodeID(id string) string {
	prefix, rest, found := strings.Cut(id, "_")
	if !found {
		return id
	}
	block, _, _ := strings.Cut(rest, "-")
	if len(block) != 8 {
		return id
	}
	return prefix + "_" + block[:shortIDDigits]
}

// shortIDDigits is how many hex digits of the UUID a project name keeps.
const shortIDDigits = 6

// sanitizeProjectName makes an arbitrary node name safe for Keystone: it drops
// non-BMP runes (the project table is utf8mb3 — an emoji makes Keystone answer
// 500), drops control characters, and collapses whitespace runs to single spaces.
// Everything else — spaces, umlauts, slashes, punctuation — Keystone accepts.
// A name that is empty or all-whitespace is rejected with 400, so it comes back
// empty here and the caller falls back to the node ID.
func sanitizeProjectName(s string) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		switch {
		// Whitespace first: newlines and tabs are control characters too, but they
		// separate words and must collapse to a space rather than vanish.
		case unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
		case r > 0xFFFF || unicode.IsControl(r) || r == utf8.RuneError:
			continue
		default:
			if pendingSpace {
				b.WriteRune(' ')
				pendingSpace = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateRunes cuts s to at most n runes (Keystone counts characters, not bytes)
// and trims a trailing space so a cut never leaves a dangling separator.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:n]), " ")
}

// descriptionPathSeparator joins the budget path and the owner in a description.
const descriptionPathSeparator = " -> "

// budgetPath returns the names of the budgets a leaf hangs under, top-down and
// without the root, e.g. ["DHBW Mannheim", "Fakultät Technik", "Prof-X"]. It is
// what an operator in Horizon needs to tell where a project belongs. A budget
// missing from budgetByID ends the walk, as does a cycle.
func budgetPath(budgetByID map[string]tree.Node, leaf tree.Node) []string {
	var path []string
	seen := map[string]struct{}{}
	for id := leaf.ParentID; id != nil && *id != tree.RootNodeID; {
		if _, dup := seen[*id]; dup {
			break
		}
		seen[*id] = struct{}{}
		budget, ok := budgetByID[*id]
		if !ok {
			break
		}
		name := budget.Name
		if name == "" {
			name = budget.ID
		}
		path = append(path, name)
		id = budget.ParentID
	}
	slices.Reverse(path)
	return path
}

// buildDescription constructs the OS project description for a leaf.
// Format: "budget -> … -> email: reason (managed project)", where the budgets are
// the leaf's path in the tree (see budgetPath) and email is the owner's address.
func buildDescription(leaf tree.Node, path []string) string {
	prefix := path
	if email := leaf.OwnerEmail(); email != "" {
		prefix = append(slices.Clip(path), email)
	}
	body := leaf.Reason
	if body == "" {
		body = "Managed by DHBW resource management. Node: " + leaf.ID
	}
	if len(prefix) > 0 {
		body = strings.Join(prefix, descriptionPathSeparator) + ": " + body
	}
	return body + managedDescriptionSuffix
}

// createOpenstackProjectForLeaf creates a new OpenStack project for an approved leaf
// and applies the full initial quota (managed fields + network defaults).
func (r *Reconciler) createOpenstackProjectForLeaf(_ context.Context, leaf tree.Node, description string) (osclient.ProjectInfo, error) {
	name := buildProjectName(leaf)

	r.log.Infow("Creating OS project for leaf",
		"node_id", leaf.ID, "project_name", name, "dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return osclient.ProjectInfo{ID: "dry-run-" + leaf.ID, Name: name}, nil
	}

	project, err := r.osClient.CreateManagedProject(name, description, r.scopeParentID, leaf.ID)
	if err != nil {
		return osclient.ProjectInfo{}, fmt.Errorf("create project: %w", err)
	}

	// Compose a full quota set: managed resources from the leaf + static defaults.
	// Static defaults (network quotas, volumes, snapshots) are driven entirely by the
	// ManagedProject definitions — no separate DefaultNetworkQuotas struct needed.
	fullQuota := ProjectQuotaToQuotaSet(r.resources(), leaf.EffectiveLimit())
	staticQuota := StaticProjectQuotaDefaults(r.resources())
	mergeStaticIntoQuotaSet(&fullQuota, staticQuota)
	fullQuota.ProjectID = project.ID

	// Retry quota set a few times: Nova/Cinder may not have propagated the new Keystone
	// project yet and returns 503 for a few seconds after creation.
	// If all attempts fail we do NOT delete the orphan — instead we return the project
	// info so the reconciler stores the OSProjectID. On the next tick the project already
	// exists and quota sync goes through syncQuota, which will keep retrying every interval
	// until Nova is healthy again. Deleting and recreating on every failure loops forever.
	const maxQuotaAttempts = 4
	const quotaRetryDelay = 6 * time.Second
	var quotaErr error
	for attempt := 1; attempt <= maxQuotaAttempts; attempt++ {
		quotaErr = r.osClient.UpdateProjectQuotas(project.ID, fullQuota)
		if quotaErr == nil {
			break
		}
		r.log.Warnw("Quota set attempt failed",
			"project_id", project.ID, "attempt", attempt, "max", maxQuotaAttempts, "error", quotaErr)
		if attempt < maxQuotaAttempts {
			time.Sleep(quotaRetryDelay)
		}
	}
	if quotaErr != nil {
		r.log.Warnw("Quota set failed; project created but quota not applied — will retry on next reconcile tick",
			"os_project_id", project.ID, "node_id", leaf.ID, "error", quotaErr)
		// Return the project so the caller persists OSProjectID. The next reconcile cycle
		// will find the project via its tag and call syncQuota, which retries quota updates.
	} else {
		r.log.Infow("OS project created and quota set",
			"node_id", leaf.ID, "os_project_id", project.ID)
	}
	return osclient.ProjectInfo{ID: project.ID, Name: project.Name, Tags: project.Tags}, nil
}

// applyOSSyncState writes the result of one sync pass onto a leaf and reports
// whether anything changed (i.e. whether the node needs persisting).
//
// The rule that matters: the measurement is only written when there WAS one.
// A failed quota-detail call used to arrive here as an empty in-use map, which
// differs from whatever was stored and therefore overwrote it with nothing.
// Since the accounting bills max(limit, in-use), the charge then silently fell
// back to the declared limit until the next successful pass — the
// shrink-after-filling loophole, reopened by a transient API error, and
// OSOvercommitted cleared along with it so the UI stopped warning too.
//
// "Not measured" is not "nothing in use". The rest of this package is careful
// about that distinction (see ProjectInUse and quotaEqual); this is the place
// where it used to be lost.
func applyOSSyncState(leaf *tree.Node, osProjectID string, m *osMeasurement) bool {
	changed := leaf.OSProjectID != osProjectID
	if m != nil {
		changed = changed || leaf.OSOvercommitted != m.overcommitted || !quotaEqual(leaf.OSInUse, m.inUse) ||
			leaf.OSServers == nil || *leaf.OSServers != m.servers ||
			leaf.OSServerLimit == nil || *leaf.OSServerLimit != m.serverLimit
	}
	if !changed {
		return false
	}
	leaf.OSProjectID = osProjectID
	if m != nil {
		leaf.OSOvercommitted = m.overcommitted
		leaf.OSInUse = m.inUse
		servers, serverLimit := m.servers, m.serverLimit
		leaf.OSServers = &servers
		leaf.OSServerLimit = &serverLimit
	}
	return true
}

// osMeasurement is what one pass read from OpenStack about a project. nil
// means it could not be read — "not measured", never "nothing in use".
type osMeasurement struct {
	overcommitted bool
	inUse         common.ProjectQuota
	// servers counts the project's servers, whatever their state, and
	// serverLimit is how many OpenStack allows.
	servers     int
	serverLimit int
}

// removeStaleImports deletes imported leaves whose OpenStack project is no longer
// in scope — each only if it is still an import when the delete happens.
func (r *Reconciler) removeStaleImports(ctx context.Context, importedByOSProjectID map[string]tree.Node, osProjectByOSID map[string]osclient.ProjectInfo, res *reconcileResult) {
	for osID, staleLeaf := range importedByOSProjectID {
		if _, stillInScope := osProjectByOSID[osID]; stillInScope {
			continue
		}
		r.log.Infow("Removing stale imported leaf (OS project gone from scope)",
			"node_id", staleLeaf.ID, "os_project_id", osID)
		if !r.cfg.DryRun {
			deleted, err := r.store.DeleteNodeIf(ctx, staleLeaf.ID, isStillImported)
			if err != nil {
				r.log.Warnw("Failed to delete stale imported leaf",
					"id", staleLeaf.ID, "error", err)
				continue
			}
			if !deleted {
				// Promoted or moved since the pass loaded it: no longer an import.
				continue
			}
		}
		res.importedRemoved++
	}
}

// persistOSProjectID records the project created for a leaf. Written onto the
// node as it is now and regardless of its status: a leaf released meanwhile
// still needs the ID, or its project could never be cleaned up.
func (r *Reconciler) persistOSProjectID(ctx context.Context, leafID, osProjectID string) {
	if _, err := r.store.UpdateNode(ctx, leafID, func(n *tree.Node) error {
		n.OSProjectID = osProjectID
		return nil
	}); err != nil {
		r.log.Warnw("Failed to persist OSProjectID on leaf", "node_id", leafID, "error", err)
	}
}

// persistOSSyncState writes what the pass measured in OpenStack onto the node as
// it is now, touching only those fields.
func (r *Reconciler) persistOSSyncState(ctx context.Context, leafID, osProjectID string, m *osMeasurement) {
	if _, err := r.store.UpdateNode(ctx, leafID, func(n *tree.Node) error {
		if !applyOSSyncState(n, osProjectID, m) {
			return tree.ErrSkipUpdate
		}
		return nil
	}); err != nil {
		r.log.Warnw("Failed to persist OS sync state on leaf", "node_id", leafID, "error", err)
	}
}

// isStillImported is the guard for deleting or refreshing an imported leaf: once
// promoted it belongs to the managed tree and the import logic must leave it be.
func isStillImported(n tree.Node) bool {
	return n.Status == tree.StatusImported
}

// syncQuota pushes the current approved limit to an existing OS project and returns
// whether the project is currently overcommitted (in-use > new limit).
// For change_pending leaves the current approved limit (leaf.EffectiveLimit()) is used —
// the proposed pending change only takes effect after manager approval.
// It also keeps name and description in sync, so renaming a node in the tree renames
// its OpenStack project on the next tick.
//
// The `measured` return says whether overcommitted/inUse mean anything. False
// covers both "we did not look" (dry run) and "we looked and could not see"
// (quota detail unavailable). Callers MUST NOT store the values in that case:
// an empty in-use map is indistinguishable from a genuine zero once written,
// and writing it undoes the whole point of billing max(limit, in-use).
// grantClient is the slice of the OpenStack client that availabilities need.
// Narrow and declared here, like scopeParentClient above, so the decision — grant
// or revoke, and say so in a dry run — can be tested without a cloud.
type grantClient interface {
	HasGrant(grant common.Grant, projectID string) (bool, error)
	// Both report whether they CHANGED anything, so a pass that alters access in
	// OpenStack leaves a line in the log and the twenty that do not stay quiet.
	AddGrant(grant common.Grant, projectID string) (bool, error)
	RemoveGrant(grant common.Grant, projectID string) (bool, error)
}

func (r *Reconciler) syncGrants(leaf tree.Node, osProjectID string) {
	syncGrants(r.osClient, r.resources(), leaf, osProjectID, r.cfg.DryRun, r.log)
}

// syncGrants brings a project's availabilities in line with what the leaf was
// granted: a network, an image or a GPU flavour it may use.
//
// Only resources IN THE CATALOGUE are ever touched, and that is the whole of the
// safety story. Flavour access and image members carry no tag saying who created
// them, so there is no way to tell our grant from one an operator made by hand —
// the only defensible rule is never to look at a target the catalogue does not
// name. An access granted on some other image stays exactly as it was, the same
// way syncGroupAssignments leaves external assignments alone.
//
// Failures are logged per resource and do not abort the pass. One unreachable
// service must not stop the other twenty projects from being reconciled, and the
// next run tries again — the desired state is in the tree, not in this call.
func syncGrants(c grantClient, defs []common.ManagedProject, leaf tree.Node, osProjectID string, dryRun bool, log *zap.SugaredLogger) {
	holds := leaf.EffectiveLimit()
	for _, def := range defs {
		if !def.IsBool() || def.Grant == nil {
			continue
		}
		wanted := holds[def.ID] == 1

		if dryRun {
			// Read-only in dry run, and it reports only DIFFERENCES: listing
			// every availability on every project would bury the handful that
			// are about to change. Reading is safe, and the difference is the
			// point of the run.
			has, err := c.HasGrant(*def.Grant, osProjectID)
			if err != nil {
				log.Warnw("Dry run: cannot read grant",
					"node_id", leaf.ID, "resource", def.ID, "error", err)
				continue
			}
			if has != wanted {
				verb := "would revoke"
				if wanted {
					verb = "would grant"
				}
				log.Infow("Dry run: "+verb+" availability",
					"node_id", leaf.ID, "os_project_id", osProjectID,
					"resource", def.ID, "grant_type", def.Grant.Type)
			}
			continue
		}

		var changed bool
		var err error
		if wanted {
			changed, err = c.AddGrant(*def.Grant, osProjectID)
		} else {
			changed, err = c.RemoveGrant(*def.Grant, osProjectID)
		}
		if err != nil {
			log.Warnw("Failed to sync availability",
				"node_id", leaf.ID, "os_project_id", osProjectID,
				"resource", def.ID, "granted", wanted, "error", err)
			continue
		}
		if changed {
			// Who may reach a network is not a detail. Logged only on a real
			// change, because this runs against every project every few minutes.
			verb := "Revoked"
			if wanted {
				verb = "Granted"
			}
			log.Infow(verb+" availability",
				"node_id", leaf.ID, "os_project_id", osProjectID,
				"resource", def.ID, "grant_type", def.Grant.Type, "target", def.Grant.Target)
		}
	}
}

func (r *Reconciler) syncQuota(leaf tree.Node, osProject osclient.ProjectInfo, description string) (*osMeasurement, error) {
	osProjectID := osProject.ID
	quotaSet := ProjectQuotaToQuotaSet(r.resources(), leaf.EffectiveLimit())

	r.log.Debugw("Syncing managed quota",
		"node_id", leaf.ID, "os_project_id", osProjectID,
		"cores", quotaSet.Cores, "ram_mb", quotaSet.RAM, "gigabytes", quotaSet.Gigabytes,
		"dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return nil, nil
	}

	if err := r.osClient.UpdateManagedQuotas(osProjectID, quotaSet); err != nil {
		return nil, fmt.Errorf("update managed quotas: %w", err)
	}

	// Name is only sent when it actually changed: an unchanged name would be a no-op
	// write every tick, and it lets an operator's manual rename of an *imported*
	// project survive until the node itself is renamed.
	updateOpts := osclient.ProjectUpdateOpts{
		BaseProjectOpts: osclient.BaseProjectOpts{Description: &description},
	}
	desiredName := buildProjectName(leaf)
	if desiredName != osProject.Name {
		updateOpts.Name = desiredName
		r.log.Infow("Renaming OS project to match node",
			"node_id", leaf.ID, "os_project_id", osProjectID,
			"old_name", osProject.Name, "new_name", desiredName)
	}
	if _, err := r.osClient.UpdateProject(osProjectID, updateOpts); err != nil {
		r.log.Warnw("Failed to update OS project name/description",
			"node_id", leaf.ID, "os_project_id", osProjectID,
			"desired_name", desiredName, "error", err)
	}

	r.syncManagedTags(leaf, osProject)

	// Overcommit check: OpenStack accepts a quota reduction below current usage but blocks
	// new resource creation. We surface this in the UI via the OSOvercommitted flag.
	detail, err := r.osClient.GetProjectQuotaDetail(osProjectID)
	if err != nil {
		// Not an error for the caller: the quota push above succeeded, so the
		// pass is not a failure. But measured=false, or the caller would store
		// "no usage" for a project it simply could not read.
		r.log.Warnw("Skipping overcommit check (quota detail unavailable); keeping the last known usage",
			"node_id", leaf.ID, "os_project_id", osProjectID, "error", err)
		return nil, nil
	}

	return &osMeasurement{
		overcommitted: IsProjectOvercommitted(r.resources(), leaf.EffectiveLimit(), detail),
		inUse:         ProjectInUse(r.resources(), detail),
		servers:       detail.InUse.Instances,
		serverLimit:   detail.Limit.Instances,
	}, nil
}

// buildDesiredMembers extracts the intended OpenStack role assignments from a leaf.
// Only user: tokens are processed — group: tokens have no direct Keystone equivalent.
// The owner receives common.OwnerOpenstackRole; AuthorizedUsers their specified one.
func buildDesiredMembers(leaf tree.Node) []osclient.DesiredMember {
	desired := make([]osclient.DesiredMember, 0, 1+len(leaf.AuthorizedUsers))
	if email := leaf.OwnerEmail(); email != "" {
		desired = append(desired, osclient.DesiredMember{
			Email: email,
			// Was hardcoded "admin", which is cloud-wide in OpenStack's default
			// policy — see the note on OwnerOpenstackRole.
			RoleName: common.OwnerOpenstackRole,
			// This is the owner's own project, so it is the sensible landing
			// place for their dashboard session (only applied when they have no
			// default project yet).
			PrimaryProject: true,
		})
	}
	for _, au := range leaf.AuthorizedUsers {
		if email, ok := strings.CutPrefix(au.Token, "user:"); ok {
			desired = append(desired, osclient.DesiredMember{
				Email:    email,
				RoleName: allowedRole(au.OpenstackRole),
			})
		}
	}
	return desired
}

// allowedRole keeps a stored role from outliving the rules it was written under.
//
// The API validates authorized_users on write, but rows persist across releases:
// an entry saved while "admin" was still selectable would otherwise keep being
// re-granted on every reconcile pass, forever, with no way to see it in the UI.
// Anything not currently offered is clamped down to the participant default
// rather than skipped — dropping it would silently revoke access instead of
// reducing it.
func allowedRole(role string) string {
	normalized := strings.ToLower(strings.TrimSpace(role))
	if slices.Contains(common.OpenstackRoles, normalized) {
		return normalized
	}
	return common.OwnerOpenstackRole
}

// syncMembers reconciles the OpenStack project's user role assignments to match the
// leaf's owner and AuthorizedUsers. Non-fatal: errors are logged
// but do not interrupt the reconciliation run.
func (r *Reconciler) syncMembers(leaf tree.Node, osProjectID string) {
	if r.cfg.DryRun {
		r.log.Debugw("Dry run: skipping member sync",
			"node_id", leaf.ID, "os_project_id", osProjectID)
		return
	}
	desired := buildDesiredMembers(leaf)
	var conflicts []osclient.PreseedConflict
	var memberSyncErr error
	conflicts, memberSyncErr = r.osClient.SyncProjectMembers(osProjectID, desired)
	if memberSyncErr != nil {
		r.log.Warnw("Member sync failed",
			"node_id", leaf.ID, "os_project_id", osProjectID, "error", memberSyncErr)
	}
	r.recordPreseedConflicts(conflicts)
}

// upsertImported creates or refreshes a synthetic imported leaf under the
// structural "unassigned" node. IDs are stable: if a leaf already exists for this
// OS project its ID is reused; otherwise a new "p_<uuid>" ID is generated.
func (r *Reconciler) upsertImported(
	ctx context.Context,
	osProject osclient.ProjectInfo,
	existing map[string]tree.Node,
	res *reconcileResult,
) {
	syntheticID := "p_" + uuid.New().String()
	if prev, ok := existing[osProject.ID]; ok {
		syntheticID = prev.ID // keep the existing ID stable across reconcile runs
	}

	var osLimit common.ProjectQuota
	// What the import uses, from the same response as its limit; nil when that
	// could not be read, and then the last known values stay.
	var measured *osMeasurement
	detail, err := r.osClient.GetProjectQuotaDetail(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch quota for import",
			"os_project_id", osProject.ID, "error", err)
		osLimit = common.ProjectQuota{}
	} else {
		osLimit = QuotaSetToProjectQuota(r.resources(), detail.Limit)
		measured = measureImport(r.resources(), osLimit, detail)
	}

	// Resolve project members. The tree model has no owner for imports (the owner
	// is assigned at promotion time) — every member is recorded as an authorized
	// user with their actual role, so nothing is lost and the promote modal can
	// suggest candidates.
	authorizedUsers := []common.AuthorizedUser{}
	members, err := r.osClient.ListProjectMemberInfo(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch member info for import, members will be empty",
			"os_project_id", osProject.ID, "error", err)
	} else {
		authorizedUsers = importMembers(members)
	}

	// Resolve group role assignments. Groups whose name resolves to a known group:
	// token are recorded as authorized users. Groups that cannot be resolved
	// (external / non-managed) are stored separately so the reconciler can preserve
	// them without exposing them to the normal management flow.
	var externalGroups []common.ExternalGroupAssignment
	groupRoles, err := r.osClient.ListProjectGroupRoles(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch group roles for import",
			"os_project_id", osProject.ID, "error", err)
	} else {
		var groups []importedGroup
		for _, g := range groupRoles {
			ig := importedGroup{id: g.GroupID, role: g.RoleName}
			if osGroup, err := r.osClient.GetGroupByID(g.GroupID); err == nil && osGroup != nil {
				ig.name = osGroup.Name
			} else {
				r.log.Debugw("Could not resolve OS group, storing as external",
					"group_id", g.GroupID, "os_project_id", osProject.ID)
			}
			groups = append(groups, ig)
		}
		members, external := importGroups(r.cfg.GroupPrefix, groups)
		authorizedUsers = append(authorizedUsers, members...)
		externalGroups = external
	}

	parent := tree.UnassignedNodeID
	leaf := tree.Node{
		ID:                       syntheticID,
		Kind:                     tree.KindProject,
		ParentID:                 &parent,
		Status:                   tree.StatusImported,
		Name:                     osProject.Name,
		Reason:                   fmt.Sprintf("OpenStack project: %s (%s)", osProject.Name, osProject.ID),
		Limit:                    osLimit,
		AuthorizedUsers:          authorizedUsers,
		ExternalGroupAssignments: externalGroups,
		History:                  []tree.HistoryEntry{},
		CreatedBy:                "System",
		OSProjectID:              osProject.ID,
		OSProjectName:            osProject.Name,
	}
	applyOSSyncState(&leaf, osProject.ID, measured)

	r.log.Infow("Upserting imported leaf",
		"node_id", syntheticID, "os_project_id", osProject.ID,
		"project_name", osProject.Name, "dry_run", r.cfg.DryRun)

	_, known := existing[osProject.ID]
	if !known {
		leaf.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if !r.cfg.DryRun {
		if known {
			// Refresh only what OpenStack says about the project, on the node as it
			// is now: history, parent, owner and flags belong to the tree, and a
			// promotion requested during this pass must survive it.
			_, err := r.store.UpdateNode(ctx, syntheticID, func(n *tree.Node) error {
				if !isStillImported(*n) || slices.Contains(n.Flags, tree.FlagPromoteOnReconcile) {
					return tree.ErrSkipUpdate
				}
				n.Name = leaf.Name
				n.Reason = leaf.Reason
				n.Limit = leaf.Limit
				n.AuthorizedUsers = leaf.AuthorizedUsers
				n.ExternalGroupAssignments = leaf.ExternalGroupAssignments
				n.OSProjectID = leaf.OSProjectID
				n.OSProjectName = leaf.OSProjectName
				applyOSSyncState(n, leaf.OSProjectID, measured)
				return nil
			})
			if err != nil {
				r.log.Warnw("Failed to update imported leaf",
					"id", syntheticID, "error", err)
				return
			}
		} else if err := r.store.UpsertNode(ctx, leaf); err != nil {
			r.log.Warnw("Failed to upsert imported leaf",
				"id", syntheticID, "error", err)
			return
		}
	}

	if _, wasKnown := existing[osProject.ID]; wasKnown {
		res.projectsSynced++
	} else {
		res.importedLeaves++
	}
}

// measureImport is what an imported project uses, read like a managed one's.
// Its limit is its OpenStack quota, so overcommitted means OpenStack already
// holds more than the quota allows.
func measureImport(resources []common.ManagedProject, limit common.ProjectQuota, detail *osclient.ProjectQuotaDetail) *osMeasurement {
	return &osMeasurement{
		overcommitted: IsProjectOvercommitted(resources, limit, detail),
		inUse:         ProjectInUse(resources, detail),
		servers:       detail.InUse.Instances,
		serverLimit:   detail.Limit.Instances,
	}
}

// importMembers turns an OpenStack project's members into authorized users.
// Accounts without an e-mail address are left out.
// The tree knows two roles, member and reader; a reader stays one, every other
// role — admin, or one a service defines — becomes member, which is what the
// project's own people get. Someone holding several roles appears once, with
// the stronger.
func importMembers(members []osclient.ProjectMemberInfo) []common.AuthorizedUser {
	out := []common.AuthorizedUser{}
	at := map[string]int{}
	for _, m := range members {
		email := strings.ToLower(strings.TrimSpace(m.Email))
		// Keystone falls back to the user name where an account has no e-mail —
		// a service account, a local user. Those are no persons of the tree, and
		// the member sync leaves accounts without an address alone.
		if _, err := mail.ParseAddress(email); err != nil {
			continue
		}
		token := common.UserPrefix + email
		role := treeRole(m.RoleName)
		if i, seen := at[token]; seen {
			if role == "member" {
				out[i].OpenstackRole = role
			}
			continue
		}
		at[token] = len(out)
		out = append(out, common.AuthorizedUser{Token: token, OpenstackRole: role})
	}
	return out
}

// importedGroup is one group role assignment of a project in OpenStack; name
// is empty when the group could not be looked up.
type importedGroup struct{ id, name, role string }

// importGroups splits a project's groups. The ones this service manages (named
// with its prefix) are its group tokens and become members; every other group
// was set up in OpenStack, is unknown to the role provider, and is kept as an
// external assignment — preserved by the reconciler, shown and removable in the
// portal, never a token that could block adopting the project. Roles become
// member or reader, as for persons; a group with several roles appears once.
func importGroups(prefix string, groups []importedGroup) ([]common.AuthorizedUser, []common.ExternalGroupAssignment) {
	var members []common.AuthorizedUser
	var external []common.ExternalGroupAssignment
	memberAt, externalAt := map[string]int{}, map[string]int{}
	for _, g := range groups {
		role := treeRole(g.role)
		if prefix != "" && strings.HasPrefix(g.name, prefix) {
			token := groupTokenForKeystoneName(prefix, g.name)
			if i, seen := memberAt[token]; seen {
				if role == "member" {
					members[i].OpenstackRole = role
				}
				continue
			}
			memberAt[token] = len(members)
			members = append(members, common.AuthorizedUser{Token: token, OpenstackRole: role})
			continue
		}
		if i, seen := externalAt[g.id]; seen {
			if role == "member" {
				external[i].Role = role
			}
			continue
		}
		externalAt[g.id] = len(external)
		external = append(external, common.ExternalGroupAssignment{GroupID: g.id, GroupName: g.name, Role: role})
	}
	return members, external
}

// treeRole maps an OpenStack role onto the two the tree knows: reader stays,
// everything else — admin, or a role a service defines — becomes member.
func treeRole(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "reader") {
		return "reader"
	}
	return "member"
}

// mergeMembers adds to a promoted leaf's authorized users everyone OpenStack
// has in the project that the leaf does not name yet, except its owner, who
// gets access as the owner. What the leaf already names is kept as it is.
func mergeMembers(leafUsers, current []common.AuthorizedUser, owner string) []common.AuthorizedUser {
	out := slices.Clone(leafUsers)
	known := map[string]bool{strings.ToLower(owner): true}
	for _, u := range leafUsers {
		known[strings.ToLower(u.Token)] = true
	}
	for _, u := range current {
		if !known[strings.ToLower(u.Token)] {
			known[strings.ToLower(u.Token)] = true
			out = append(out, u)
		}
	}
	return out
}

// relationSeparator stands in for "#" in Keystone group names:
// group:wwi23seb#dozent becomes "<prefix>wwi23seb--dozent". A group ID that
// itself ends in "--<relation>" would read back as a relation; relation names
// are lowercase words, so that takes a deliberately odd group name.
const relationSeparator = "--"

// keystoneGroupName is the Keystone group a group token is provisioned as.
// Plain groups keep the name they always had, so no existing group is renamed.
func keystoneGroupName(prefix, token string) string {
	group, relation := common.SplitGroupToken(token)
	name := prefix + strings.TrimPrefix(group, common.GroupPrefix)
	if relation != common.RelationMember {
		name += relationSeparator + relation
	}
	return name
}

// groupTokenForKeystoneName maps a Keystone group found on an imported project
// back to a token: one of ours (carrying the prefix) to the token it was
// provisioned for, a foreign one to "group:<name>" as before.
func groupTokenForKeystoneName(prefix, name string) string {
	base, ours := strings.CutPrefix(name, prefix)
	if !ours || prefix == "" {
		return common.GroupPrefix + name
	}
	if i := strings.LastIndex(base, relationSeparator); i > 0 {
		relation := base[i+len(relationSeparator):]
		if isRelationName(relation) {
			return common.JoinGroupToken(common.GroupPrefix+base[:i], relation)
		}
	}
	return common.GroupPrefix + base
}

// isRelationName matches the role-provider's rule for relation names.
func isRelationName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// collectGroupTokens returns the set of unique group: tokens referenced by any
// AuthorizedUsers entry across the given leaves.
func collectGroupTokens(leaves []tree.Node) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, leaf := range leaves {
		for _, au := range leaf.AuthorizedUsers {
			if strings.HasPrefix(au.Token, "group:") {
				tokens[au.Token] = struct{}{}
			}
		}
	}
	return tokens
}

// syncGroups ensures a Keystone group exists for every group: token referenced
// in active leaves, populates each group's membership from the role provider,
// and returns a map of groupToken → Keystone group ID for use in project assignment.
// Non-fatal: errors for individual groups are logged but do not abort the run.
func (r *Reconciler) syncGroups(ctx context.Context, activeLeaves []tree.Node, res *reconcileResult) map[string]string {
	if r.osClient == nil {
		return nil
	}

	groupTokens := collectGroupTokens(activeLeaves)
	groupTokenToOSID := make(map[string]string, len(groupTokens))

	for token := range groupTokens {
		// Groups have no scope parent and carry no tags, so the prefix is what
		// marks them as ours and keeps them out of the way of foreign groups.
		osGroupName := keystoneGroupName(r.cfg.GroupPrefix, token)

		// Find or create the Keystone group.
		existing, err := r.osClient.FindGroupByName(osGroupName)
		if err != nil {
			r.log.Warnw("Could not look up OS group, skipping", "group", osGroupName, "error", err)
			continue
		}

		var groupID string
		if existing != nil {
			groupID = existing.ID
		} else {
			r.log.Infow("Creating OS group", "group", osGroupName, "dry_run", r.cfg.DryRun)
			if r.cfg.DryRun {
				res.groupsCreated++
				continue
			}
			created, err := r.osClient.CreateGroup(osGroupName, "Managed by openstack-management-api")
			if err != nil {
				r.log.Warnw("Failed to create OS group", "group", osGroupName, "error", err)
				continue
			}
			groupID = created.ID
			res.groupsCreated++
		}

		groupTokenToOSID[token] = groupID

		// Sync group memberships when a role provider is available.
		if r.roleProvider != nil {
			r.syncGroupMembers(ctx, token, osGroupName, groupID, res)
		}
	}

	return groupTokenToOSID
}

// syncGroupMembers reconciles the Keystone group's user list against the users
// returned by the role provider for that group token.
// Non-fatal: errors for individual users are logged and skipped.
func (r *Reconciler) syncGroupMembers(ctx context.Context, groupToken, groupName, groupID string, res *reconcileResult) {
	desiredEmails, err := r.roleProvider.GetGroupUsers(ctx, groupToken)
	if err != nil {
		r.log.Warnw("Could not fetch group users from role provider",
			"group", groupName, "error", err)
		return
	}

	// Resolve desired emails to Keystone user IDs, creating accounts as needed.
	desiredUserIDs := make(map[string]struct{}, len(desiredEmails))
	for _, email := range desiredEmails {
		if r.cfg.DryRun {
			r.log.Debugw("Dry run: would ensure group member", "group", groupName, "email", email)
			continue
		}
		user, err := r.osClient.FindOrCreateUser(email)
		if err != nil {
			var conflict *osclient.PreseedConflict
			if errors.As(err, &conflict) {
				r.log.Warnw("Pre-seeding conflict — group membership NOT set",
					"group", groupName, "email", email, "reason", conflict.Reason)
				r.recordPreseedConflicts([]osclient.PreseedConflict{*conflict})
			} else {
				r.log.Warnw("Could not find/create user for group membership",
					"group", groupName, "email", email, "error", err)
			}
			continue
		}
		desiredUserIDs[user.ID] = struct{}{}
	}

	if r.cfg.DryRun {
		res.groupsSynced++
		return
	}

	// Fetch current group members.
	currentUserIDs, err := r.osClient.ListGroupUsers(groupID)
	if err != nil {
		r.log.Warnw("Could not list current group members, skipping sync",
			"group", groupName, "group_id", groupID, "error", err)
		return
	}
	currentSet := make(map[string]struct{}, len(currentUserIDs))
	for _, id := range currentUserIDs {
		currentSet[id] = struct{}{}
	}

	// Add missing members.
	for id := range desiredUserIDs {
		if _, ok := currentSet[id]; ok {
			continue
		}
		if err := r.osClient.AddUserToGroup(groupID, id); err != nil {
			r.log.Warnw("Failed to add user to group",
				"group", groupName, "user_id", id, "error", err)
		} else {
			r.log.Infow("Added user to group", "group", groupName, "user_id", id)
		}
	}

	// Remove users no longer in the desired set.
	for id := range currentSet {
		if _, ok := desiredUserIDs[id]; ok {
			continue
		}
		if err := r.osClient.RemoveUserFromGroup(groupID, id); err != nil {
			r.log.Warnw("Failed to remove user from group",
				"group", groupName, "user_id", id, "error", err)
		} else {
			r.log.Infow("Removed user from group", "group", groupName, "user_id", id)
		}
	}

	res.groupsSynced++
}

// syncGroupAssignments reconciles the Keystone group role assignments for a
// single project based on the group: tokens in the leaf's AuthorizedUsers.
// Non-fatal: errors are logged and skipped.
func (r *Reconciler) syncGroupAssignments(leaf tree.Node, osProjectID string, groupTokenToOSID map[string]string) {
	if r.osClient == nil || r.cfg.DryRun {
		if r.cfg.DryRun {
			r.log.Debugw("Dry run: skipping group assignment sync",
				"node_id", leaf.ID, "os_project_id", osProjectID)
		}
		return
	}

	// Build desired group assignments for this leaf. A group of the leaf whose
	// Keystone group this pass could not look up leaves the desired set
	// incomplete; the cleanup below would then remove that group's access, so
	// the leaf waits for the next pass instead.
	type desired struct{ groupID, roleName string }
	var desiredList []desired
	for _, au := range leaf.AuthorizedUsers {
		if !strings.HasPrefix(au.Token, common.GroupPrefix) {
			continue
		}
		id, ok := groupTokenToOSID[au.Token]
		if !ok {
			r.log.Warnw("Group not resolved in this pass, skipping group assignment sync",
				"node_id", leaf.ID, "group", au.Token)
			return
		}
		desiredList = append(desiredList, desired{groupID: id, roleName: au.OpenstackRole})
	}
	// External groups have no delegation token — add them by their OS group ID directly
	// so they are always preserved and never removed by the cleanup pass below.
	for _, eg := range leaf.ExternalGroupAssignments {
		desiredList = append(desiredList, desired{groupID: eg.GroupID, roleName: eg.Role})
	}

	// Build desired set: groupID → roleName.
	desiredSet := make(map[string]string, len(desiredList))
	for _, d := range desiredList {
		desiredSet[d.groupID] = d.roleName
	}

	// Fetch current group assignments for the project.
	current, err := r.osClient.ListProjectGroupRoles(osProjectID)
	if err != nil {
		r.log.Warnw("Could not list current group project roles, skipping group assignment sync",
			"node_id", leaf.ID, "os_project_id", osProjectID, "error", err)
		return
	}
	currentSet := make(map[string]string, len(current))     // groupID → roleName
	currentRoleIDs := make(map[string]string, len(current)) // groupID → roleID
	for _, c := range current {
		currentSet[c.GroupID] = c.RoleName
		currentRoleIDs[c.GroupID] = c.RoleID
	}

	// Add or update missing assignments.
	for groupID, roleName := range desiredSet {
		if cur, ok := currentSet[groupID]; ok && strings.EqualFold(cur, roleName) {
			continue // already correct
		}
		// Remove stale role if the group is assigned with a different role.
		if _, ok := currentSet[groupID]; ok {
			if err := r.osClient.UnassignGroupFromProject(osProjectID, groupID, currentRoleIDs[groupID]); err != nil {
				r.log.Warnw("Failed to remove stale group role from project",
					"group_id", groupID, "os_project_id", osProjectID, "error", err)
			}
		}
		role, err := r.osClient.FindRoleByName(roleName)
		if err != nil {
			r.log.Warnw("Role not found in OpenStack, skipping group assignment",
				"group_id", groupID, "role", roleName, "error", err)
			continue
		}
		if err := r.osClient.AssignGroupToProject(osProjectID, groupID, role.ID); err != nil {
			r.log.Warnw("Failed to assign group to project",
				"group_id", groupID, "role", roleName, "os_project_id", osProjectID, "error", err)
		} else {
			r.log.Infow("Assigned group to project",
				"group_id", groupID, "role", roleName, "os_project_id", osProjectID)
		}
	}

	// Remove group assignments no longer desired.
	for groupID, roleID := range currentRoleIDs {
		if _, keep := desiredSet[groupID]; keep {
			continue
		}
		if err := r.osClient.UnassignGroupFromProject(osProjectID, groupID, roleID); err != nil {
			r.log.Warnw("Failed to remove group from project",
				"group_id", groupID, "os_project_id", osProjectID, "error", err)
		} else {
			r.log.Infow("Removed group from project",
				"group_id", groupID, "os_project_id", osProjectID)
		}
	}
}
