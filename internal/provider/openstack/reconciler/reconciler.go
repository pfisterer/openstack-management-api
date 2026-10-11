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
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// TreeSync is what the reconciler may do with the tree: read what is wanted,
// and report what it found in OpenStack. It is the method set of
// *tree.ProviderSync, which owns the rules for how a report lands on a node;
// the reconciler only decides what it observed.
type TreeSync interface {
	Leaves(ctx context.Context, statuses []string) ([]model.Node, error)
	Budgets(ctx context.Context) ([]model.Node, error)
	RecordExternalID(ctx context.Context, leafID, externalID string) error
	RecordObservation(ctx context.Context, leafID, externalID string, o *tree.Observation) error
	MarkArchived(ctx context.Context, leafID string) error
	RecordPurgeProgress(ctx context.Context, leafID string, r tree.PurgeReport) error
	RemoveRetired(ctx context.Context, leafID string) (bool, error)
	RemoveImport(ctx context.Context, leafID string) (bool, error)
	AddImport(ctx context.Context, leaf model.Node) error
	RefreshImport(ctx context.Context, leaf model.Node, o *tree.Observation) error
	Promote(ctx context.Context, leafID string, current []common.AuthorizedUser) (bool, error)
	// Changes receives when what is wanted changed; a run then starts right away instead of at the next interval.
	Changes() <-chan struct{}
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
	nodes    TreeSync
	osClient *osclient.OpenStackClient
	cfg      Config
	// catalog is read through resources(): root admins add and withdraw
	// availabilities while the reconciler runs (package catalog).
	catalog      osclient.ManagedCatalog
	roleProvider roles.Provider
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

	// usageBooks and usageMeter, when set, record each finished day's
	// consumption after a pass: the books say which days are missing and
	// keep the rows, the meter reads OpenStack.
	usageBooks UsageBooks
	usageMeter UsageMeter

	// problems keeps what the recent runs logged at warning level and above.
	problems *problemRecorder
}

// New creates a Reconciler. managedProjects must match AppConfiguration.ProjectDefinitions
// and are used to drive quota translation and overcommit detection.
// roleProvider is optional (may be nil); when set it is used to resolve group memberships
// so Keystone groups can be populated during reconciliation.
func New(
	nodes TreeSync,
	osClient *osclient.OpenStackClient,
	cfg Config,
	managedProjects []osclient.ManagedResource,
	roleProvider roles.Provider,
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
		nodes:        nodes,
		osClient:     osClient,
		cfg:          cfg,
		catalog:      osclient.StaticManaged(managedProjects),
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

	// Run once immediately on startup so the state is consistent from the first request. It covers whatever changed before, bootstrap included.
	select {
	case <-r.nodes.Changes():
	default:
	}
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
		case <-r.nodes.Changes():
			r.runOnce(ctx)
		}
	}
}

// Trigger requests an immediate reconciliation run. Non-blocking: if a run is already
// queued the second signal is silently dropped. A trigger during a run is kept and
// starts the next run once this one ends, so a change made mid-run is not lost:
// the queued run has not read the state yet.
//
// Changes made through the API start a run the same way, through TreeSync.Changes. That holds per process — with more than one replica, only the one that served the request would run.
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
	if r.usageBooks != nil && r.usageMeter != nil {
		if n, uerr := r.recordUsage(ctx); uerr != nil {
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
func (r *Reconciler) listLeaves(ctx context.Context, statuses []string) ([]model.Node, error) {
	return r.nodes.Leaves(ctx, statuses)
}

// Reconcile performs one full two-way sync and returns a summary.
// Safe to call directly without Start (e.g. from tests).
func (r *Reconciler) Reconcile(ctx context.Context) (reconcileResult, error) {
	var res reconcileResult

	// ── Phase 1: load state from both sides ──────────────────────────────────

	activeLeaves, err := r.listLeaves(ctx, model.ReconcilableStatuses)
	if err != nil {
		return res, fmt.Errorf("load active leaves: %w", err)
	}

	// allKnownLeaves covers every real status so Phase 5 can tell whether a tagged
	// OS project is already tracked (in any state) before deciding to import it.
	// This prevents projects from being re-imported when their leaf is e.g.
	// pending, rejected, or released.
	allKnownLeaves, err := r.listLeaves(ctx, model.KnownStatuses)
	if err != nil {
		return res, fmt.Errorf("load all known leaves: %w", err)
	}

	// releasedLeafByID is a subset of allKnownLeaves used in Phase 5 to tag,
	// archive or delete OS projects whose leaf has been released (or archived
	// since).
	releasedLeafByID := make(map[string]model.Node, len(allKnownLeaves))
	for _, leaf := range allKnownLeaves {
		if model.IsRetiredStatus(leaf.Status) {
			releasedLeafByID[leaf.ID] = leaf
		}
	}

	existingImported, err := r.listLeaves(ctx, []string{model.StatusImported})
	if err != nil {
		return res, fmt.Errorf("load imported leaves: %w", err)
	}

	// Every budget, so a project's description can name its path in the tree.
	budgets, err := r.nodes.Budgets(ctx)
	if err != nil {
		return res, fmt.Errorf("load budgets: %w", err)
	}
	budgetByID := make(map[string]model.Node, len(budgets))
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

	leafByID := make(map[string]model.Node, len(activeLeaves))
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
	importedByOSProjectID := make(map[string]model.Node, len(existingImported))
	for _, leaf := range existingImported {
		if osID(leaf) != "" {
			importedByOSProjectID[osID(leaf)] = leaf
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

	// What uses each availability, for all projects in one read; nil when it
	// could not be read.
	grantUse := r.readGrantUse()

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
			leaf.EditBinding(osclient.Provider, func(b *model.Binding) { b.ExternalID = created.ID })
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
			if m != nil {
				m.GrantUse = projectGrantUse(r.resources(), grantUse, osProject.ID)
			}
			if tree.Observe(&leaf, osclient.Provider, osProject.ID, m) && !r.cfg.DryRun {
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
		r.upsertImported(ctx, osProject, importedByOSProjectID, grantUse, &res)
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

// UseCatalog replaces the catalogue given to New with one that changes at
// runtime. Called before Start.
func (r *Reconciler) UseCatalog(c osclient.ManagedCatalog) { r.catalog = c }

func (r *Reconciler) resources() []osclient.ManagedResource { return r.catalog.Managed() }
