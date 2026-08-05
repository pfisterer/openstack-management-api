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
	"slices"
	"strings"
	"sync"
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

	// NoDelete prevents all destructive operations. When true:
	//   - Released OS projects are always tagged for pending deletion (never deleted), regardless of DeleteReleasedProjects.
	//   - Stale imported leaves are kept (not removed from the database).
	//   - Orphaned managed Keystone users have their description updated to OrphanedUserFlagDescription instead of being deleted.
	//   - Group membership removals and project member removals are skipped.
	//   - Group role un-assignments from projects are skipped.
	// This is intended as a "phase 1" safe mode while the reconciler is being introduced.
	NoDelete bool

	// DeleteReleasedProjects controls what happens to OS projects whose leaf is released.
	// When true the project is deleted from OpenStack immediately.
	// When false (default) the project is kept and tagged with a pending-deletion date and
	// contact info so external workflow tools can drive the actual cleanup.
	// Ignored when NoDelete is true.
	DeleteReleasedProjects bool
	// PendingDeletionGraceDays is added to today's date to compute the deletion date tag
	// written to released projects when DeleteReleasedProjects is false. Default: 30.
	PendingDeletionGraceDays int
	// PendingDeletionTagPrefix is the tag prefix for the scheduled deletion date.
	// Full tag format: "<prefix><YYYY-MM-DD>". Default: "pending-deletion:".
	PendingDeletionTagPrefix string
	// ContactTagPrefix is the prefix for tags that record owner contact addresses.
	// Default: "contact:".
	ContactTagPrefix string
}

// Status is returned by GetStatus to report the outcome of the last reconciliation run.
type Status struct {
	LastRunAt                 time.Time `json:"last_run_at"`
	LastError                 string    `json:"last_error,omitempty"`
	ProjectsSynced            int       `json:"projects_synced"`
	ProjectsCreated           int       `json:"projects_created"`
	ImportedLeaves            int       `json:"imported_leaves"`
	ImportedRemoved           int       `json:"imported_removed"`
	OrphanedUsersRemoved      int       `json:"orphaned_users_removed"`
	GroupsCreated             int       `json:"groups_created"`
	GroupsSynced              int       `json:"groups_synced"`
	ProjectsTaggedForDeletion int       `json:"projects_tagged_for_deletion"`
	ProjectsDeleted           int       `json:"projects_deleted"`
	ProjectsPromoted          int       `json:"projects_promoted"`
	Running                   bool      `json:"running"`
	// PreseedConflicts lists users whose Keystone account could not be resolved
	// without guessing, so their role was NOT assigned. These need a human — a
	// wrong guess creates an account nobody logs into while the role points
	// nowhere, which is invisible until someone reports missing access.
	PreseedConflicts []osclient.PreseedConflict `json:"preseed_conflicts,omitempty"`
}

// Reconciler orchestrates the two-way sync.
type Reconciler struct {
	store           ReconcilerStore
	osClient        *osclient.OpenStackClient
	cfg             Config
	managedProjects []common.ManagedProject
	roleProvider    common.RoleProvider
	log             *zap.SugaredLogger

	mu      sync.RWMutex
	status  Status
	trigger chan struct{}

	// scopeParentID is the effective scope parent resolved from Config
	// (ScopeParentID, or ScopeParentName looked up / created in OpenStack).
	// Set at the start of every Reconcile run; only accessed from the
	// single-threaded reconcile loop.
	scopeParentID string
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
	return &Reconciler{
		store:           store,
		osClient:        osClient,
		cfg:             cfg,
		managedProjects: managedProjects,
		roleProvider:    roleProvider,
		log:             log,
		trigger:         make(chan struct{}, 1),
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
// queued the second signal is silently dropped.
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
	return r.status
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

	result, err := r.Reconcile(ctx)

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
		r.status.OrphanedUsersRemoved = result.orphanedUsersRemoved
		r.status.GroupsCreated = result.groupsCreated
		r.status.GroupsSynced = result.groupsSynced
		r.status.ProjectsTaggedForDeletion = result.projectsTaggedForDeletion
		r.status.ProjectsDeleted = result.projectsDeleted
		r.status.ProjectsPromoted = result.projectsPromoted
		r.log.Infow("Reconciliation complete",
			"synced", result.projectsSynced,
			"created", result.projectsCreated,
			"imported", result.importedLeaves,
			"imported_removed", result.importedRemoved,
			"orphaned_users_removed", result.orphanedUsersRemoved,
			"groups_created", result.groupsCreated,
			"groups_synced", result.groupsSynced,
			"tagged_for_deletion", result.projectsTaggedForDeletion,
			"deleted", result.projectsDeleted,
			"promoted", result.projectsPromoted)
	}
	r.mu.Unlock()
}

type reconcileResult struct {
	projectsSynced            int
	projectsCreated           int
	importedLeaves            int
	importedRemoved           int
	orphanedUsersRemoved      int
	groupsCreated             int
	groupsSynced              int
	projectsTaggedForDeletion int
	projectsDeleted           int
	projectsPromoted          int
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

	// releasedLeafByID is a subset of allKnownLeaves used in Phase 5 to tag or
	// delete OS projects whose leaf has been released.
	releasedLeafByID := make(map[string]tree.Node, len(allKnownLeaves))
	for _, leaf := range allKnownLeaves {
		if leaf.Status == tree.StatusReleased {
			releasedLeafByID[leaf.ID] = leaf
		}
	}

	existingImported, err := r.listLeaves(ctx, []string{tree.StatusImported})
	if err != nil {
		return res, fmt.Errorf("load imported leaves: %w", err)
	}

	scopeParentID, err := r.ensureScopeParent()
	if err != nil {
		return res, fmt.Errorf("resolve scope parent: %w", err)
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

	for _, leaf := range activeLeaves {
		osProject, hasProject := osProjectByResourceID[leaf.ID]
		if !hasProject {
			created, err := r.createOpenstackProjectForLeaf(ctx, leaf)
			if err != nil {
				r.log.Warnw("Failed to create OS project for leaf", "node_id", leaf.ID, "error", err)
				continue
			}
			res.projectsCreated++
			leaf.OSProjectID = created.ID
			if !r.cfg.DryRun {
				if err := r.store.UpsertNode(ctx, leaf); err != nil {
					r.log.Warnw("Failed to persist OSProjectID on leaf", "node_id", leaf.ID, "error", err)
				}
			}
			r.syncMembers(leaf, created.ID)
			r.syncGroupAssignments(leaf, created.ID, groupTokenToOSID)
		} else {
			overcommitted, err := r.syncQuota(leaf, osProject)
			if err != nil {
				r.log.Warnw("Failed to sync quota for leaf", "node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
			if leaf.OSProjectID != osProject.ID || leaf.OSOvercommitted != overcommitted {
				leaf.OSProjectID = osProject.ID
				leaf.OSOvercommitted = overcommitted
				if !r.cfg.DryRun {
					if err := r.store.UpsertNode(ctx, leaf); err != nil {
						r.log.Warnw("Failed to persist OS sync state on leaf", "node_id", leaf.ID, "error", err)
					}
				}
			}
			r.syncMembers(leaf, osProject.ID)
			r.syncGroupAssignments(leaf, osProject.ID, groupTokenToOSID)
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
				// Leaf was released: tag the project for pending deletion or delete it.
				r.handleReleasedProject(osProject, releasedLeaf, &res)
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

	// Clean up imported leaves whose OS projects no longer exist.
	for osID, staleLeaf := range importedByOSProjectID {
		if _, stillExists := osProjectByOSID[osID]; !stillExists {
			if r.cfg.NoDelete {
				r.log.Infow("NoDelete: skipping removal of stale imported leaf (OS project gone)",
					"node_id", staleLeaf.ID, "os_project_id", osID)
				continue
			}
			r.log.Infow("Removing stale imported leaf (OS project gone)",
				"node_id", staleLeaf.ID, "os_project_id", osID)
			if !r.cfg.DryRun {
				if err := r.store.DeleteNodes(ctx, []string{staleLeaf.ID}); err != nil {
					r.log.Warnw("Failed to delete stale imported leaf",
						"id", staleLeaf.ID, "error", err)
				}
			}
			res.importedRemoved++
		}
	}

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
	r.pruneOrphanedUsers(&res)

	return res, nil
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
			if err := r.osClient.TagProjectForPromotion(osProject.ID, leaf.ID, osProject.Tags); err != nil {
				r.log.Warnw("Failed to tag OS project for promotion",
					"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
		}

		promoted := leaf
		promoted.Status = tree.StatusPending
		promoted.Flags = removeFlag(leaf.Flags, tree.FlagPromoteOnReconcile)

		if !r.cfg.DryRun {
			if err := r.store.UpsertNode(ctx, promoted); err != nil {
				r.log.Warnw("Failed to persist promoted leaf",
					"node_id", leaf.ID, "error", err)
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
		if r.cfg.NoDelete {
			if u.Description == osclient.OrphanedUserFlagDescription {
				continue // already flagged
			}
			r.log.Infow("NoDelete: flagging orphaned managed user via description",
				"user_id", u.ID, "name", u.Name, "dry_run", r.cfg.DryRun)
			if !r.cfg.DryRun {
				if err := r.osClient.UpdateUserDescription(u.ID, osclient.OrphanedUserFlagDescription); err != nil {
					r.log.Warnw("Failed to flag orphaned managed user",
						"user_id", u.ID, "name", u.Name, "error", err)
				}
			}
			res.orphanedUsersRemoved++
			continue
		}
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

// handleReleasedProject either deletes or tags an OS project whose leaf has been
// released, depending on Config.DeleteReleasedProjects.
//
// When deletion is disabled (default) the project receives:
//   - a pending-deletion date tag (<PendingDeletionTagPrefix><YYYY-MM-DD>)
//   - a contact tag with the owner's email (<ContactTagPrefix><email>)
//
// The tagging is idempotent: if the pending-deletion tag is already present the
// project is left unchanged on subsequent reconcile runs.
func (r *Reconciler) handleReleasedProject(osProject osclient.ProjectInfo, leaf tree.Node, res *reconcileResult) {
	if r.cfg.DeleteReleasedProjects && !r.cfg.NoDelete {
		r.log.Infow("Deleting OS project for released leaf",
			"os_project_id", osProject.ID, "node_id", leaf.ID, "dry_run", r.cfg.DryRun)
		if !r.cfg.DryRun {
			if err := r.osClient.DeleteProject(osProject.ID); err != nil {
				r.log.Warnw("Failed to delete OS project for released leaf",
					"os_project_id", osProject.ID, "node_id", leaf.ID, "error", err)
				return
			}
		}
		res.projectsDeleted++
		return
	}

	// Check idempotency: skip if the pending-deletion tag is already set.
	for _, tag := range osProject.Tags {
		if strings.HasPrefix(tag, r.cfg.PendingDeletionTagPrefix) {
			return
		}
	}

	graceDays := r.cfg.PendingDeletionGraceDays
	if graceDays <= 0 {
		graceDays = 30
	}
	deletionDate := time.Now().AddDate(0, 0, graceDays).Format("2006-01-02")

	// Rebuild the tag list: keep existing tags (minus stale contact tags), then append
	// the new pending-deletion date tag and the owner contact tag.
	newTags := make([]string, 0, len(osProject.Tags)+2)
	for _, tag := range osProject.Tags {
		if !strings.HasPrefix(tag, r.cfg.ContactTagPrefix) {
			newTags = append(newTags, tag)
		}
	}
	newTags = append(newTags, r.cfg.PendingDeletionTagPrefix+deletionDate)
	if email := leaf.OwnerEmail(); email != "" {
		newTags = append(newTags, r.cfg.ContactTagPrefix+email)
	}

	r.log.Infow("Tagging OS project for pending deletion",
		"os_project_id", osProject.ID, "node_id", leaf.ID,
		"deletion_date", deletionDate, "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if _, err := r.osClient.UpdateProject(osProject.ID, osclient.ProjectUpdateOpts{
			Tags: &newTags,
		}); err != nil {
			r.log.Warnw("Failed to tag OS project for pending deletion",
				"os_project_id", osProject.ID, "node_id", leaf.ID, "error", err)
			return
		}
	}
	res.projectsTaggedForDeletion++
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
	return id, nil
}

// keystoneProjectNameMaxLen is Keystone's hard limit for project names: the API
// schema caps "name" at 64 characters and rejects anything longer with 400.
const keystoneProjectNameMaxLen = 64

// managedDescriptionSuffix is appended to every managed project's description so a
// project is recognisable as ours in Horizon, where tags are not shown. It carries
// no meaning for the reconciler — identification runs on tags alone.
const managedDescriptionSuffix = " (managed project)"

// buildProjectName constructs the OS project name for a leaf: what the leaf is
// called, with a short form of its ID appended, e.g. "Cloud Computing [p_7ad31c42]".
//
// "What it is called" falls back to the purpose, because a project request has
// no name field at all — only a purpose. Without the fallback every requested
// project ended up named after its bare node ID ("p_7ad31c42-21e7-…"), which is
// what a user sees in Horizon and Skyline and cannot tell apart from any other.
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
	if room < 1 {
		// Pathologically long ID: drop the name, keep the identifying part.
		return truncateRunes(id, keystoneProjectNameMaxLen)
	}
	return truncateRunes(name, room) + suffix
}

// shortNodeID shortens a node ID for use inside a project name. Node IDs are a
// kind prefix plus a UUID ("p_7ad31c42-21e7-4fbd-aa3e-15a4660449be"), and 36
// characters of that are noise beside a six-letter purpose — the name is read by
// people, in Horizon and Skyline, next to dozens of others. Only the first UUID
// block is kept, the same trade git makes with short commit hashes.
//
// Uniqueness survives it: the suffix only has to separate projects that share a
// NAME, and two of those collide only if their IDs also agree in the first eight
// hex digits. The kind prefix stays so the suffix still reads as a node ID and
// can be pasted into a search.
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
	return prefix + "_" + block
}

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

// buildDescription constructs the OS project description for a leaf.
// Format: "email: reason (managed project)" where email is the owner's address.
func buildDescription(leaf tree.Node) string {
	email := leaf.OwnerEmail()
	switch {
	case email != "" && leaf.Reason != "":
		return email + ": " + leaf.Reason + managedDescriptionSuffix
	case leaf.Reason != "":
		return leaf.Reason + managedDescriptionSuffix
	}
	return fmt.Sprintf("Managed by DHBW resource management. Node: %s%s", leaf.ID, managedDescriptionSuffix)
}

// createOpenstackProjectForLeaf creates a new OpenStack project for an approved leaf
// and applies the full initial quota (managed fields + network defaults).
func (r *Reconciler) createOpenstackProjectForLeaf(_ context.Context, leaf tree.Node) (osclient.ProjectInfo, error) {
	name := buildProjectName(leaf)
	description := buildDescription(leaf)

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
	fullQuota := ProjectQuotaToQuotaSet(r.managedProjects, leaf.Limit)
	staticQuota := StaticProjectQuotaDefaults(r.managedProjects)
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
	}

	r.log.Infow("OS project created and quota set",
		"node_id", leaf.ID, "os_project_id", project.ID)
	return osclient.ProjectInfo{ID: project.ID, Name: project.Name, Tags: project.Tags}, nil
}

// syncQuota pushes the current approved limit to an existing OS project and returns
// whether the project is currently overcommitted (in-use > new limit).
// For change_pending leaves the current approved limit (leaf.Limit) is used —
// the proposed pending change only takes effect after manager approval.
// It also keeps name and description in sync, so renaming a node in the tree renames
// its OpenStack project on the next tick.
func (r *Reconciler) syncQuota(leaf tree.Node, osProject osclient.ProjectInfo) (overcommitted bool, err error) {
	osProjectID := osProject.ID
	quotaSet := ProjectQuotaToQuotaSet(r.managedProjects, leaf.Limit)

	r.log.Debugw("Syncing managed quota",
		"node_id", leaf.ID, "os_project_id", osProjectID,
		"cores", quotaSet.Cores, "ram_mb", quotaSet.RAM, "gigabytes", quotaSet.Gigabytes,
		"dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return false, nil
	}

	if err := r.osClient.UpdateManagedQuotas(osProjectID, quotaSet); err != nil {
		return false, fmt.Errorf("update managed quotas: %w", err)
	}

	// Name is only sent when it actually changed: an unchanged name would be a no-op
	// write every tick, and it lets an operator's manual rename of an *imported*
	// project survive until the node itself is renamed.
	description := buildDescription(leaf)
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

	// Overcommit check: OpenStack accepts a quota reduction below current usage but blocks
	// new resource creation. We surface this in the UI via the OSOvercommitted flag.
	detail, err := r.osClient.GetProjectQuotaDetail(osProjectID)
	if err != nil {
		r.log.Warnw("Skipping overcommit check (quota detail unavailable)",
			"node_id", leaf.ID, "os_project_id", osProjectID, "error", err)
		return false, nil
	}

	return IsProjectOvercommitted(r.managedProjects, leaf.Limit, detail), nil
}

// buildDesiredMembers extracts the intended OpenStack role assignments from a leaf.
// Only user: tokens are processed — group: tokens have no direct Keystone equivalent.
// The owner receives the admin role; AuthorizedUsers their specified OpenStack role.
func buildDesiredMembers(leaf tree.Node) []osclient.DesiredMember {
	desired := make([]osclient.DesiredMember, 0, 1+len(leaf.AuthorizedUsers))
	if email := leaf.OwnerEmail(); email != "" {
		desired = append(desired, osclient.DesiredMember{
			Email:    email,
			RoleName: "admin",
		})
	}
	for _, au := range leaf.AuthorizedUsers {
		if email, ok := strings.CutPrefix(au.Token, "user:"); ok {
			desired = append(desired, osclient.DesiredMember{
				Email:    email,
				RoleName: au.OpenstackRole,
			})
		}
	}
	return desired
}

// syncMembers reconciles the OpenStack project's user role assignments to match the
// leaf's owner (admin) and AuthorizedUsers. Non-fatal: errors are logged
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
	if r.cfg.NoDelete {
		conflicts, memberSyncErr = r.osClient.EnsureProjectMembers(osProjectID, desired)
	} else {
		conflicts, memberSyncErr = r.osClient.SyncProjectMembers(osProjectID, desired)
	}
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
	detail, err := r.osClient.GetProjectQuotaDetail(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch quota for import",
			"os_project_id", osProject.ID, "error", err)
		osLimit = common.ProjectQuota{}
	} else {
		osLimit = QuotaSetToProjectQuota(r.managedProjects, detail.Limit)
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
		for _, m := range members {
			authorizedUsers = append(authorizedUsers, common.AuthorizedUser{
				Token:         "user:" + m.Email,
				OpenstackRole: m.RoleName,
			})
		}
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
		for _, g := range groupRoles {
			osGroup, err := r.osClient.GetGroupByID(g.GroupID)
			if err != nil || osGroup == nil {
				// Can't resolve the group name — treat as external and preserve by ID.
				r.log.Debugw("Could not resolve OS group, storing as external",
					"group_id", g.GroupID, "os_project_id", osProject.ID)
				externalGroups = append(externalGroups, common.ExternalGroupAssignment{
					GroupID: g.GroupID,
					Role:    g.RoleName,
				})
				continue
			}
			authorizedUsers = append(authorizedUsers, common.AuthorizedUser{
				Token:         "group:" + osGroup.Name,
				OpenstackRole: g.RoleName,
			})
		}
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

	// Preserve existing history, flags, promote state and creation time so they
	// aren't wiped on every reconcile cycle.
	if prev, ok := existing[osProject.ID]; ok {
		leaf.History = prev.History
		leaf.Flags = prev.Flags
		leaf.ParentID = prev.ParentID
		leaf.Owner = prev.Owner
		leaf.CreatedAt = prev.CreatedAt
	} else {
		leaf.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	r.log.Infow("Upserting imported leaf",
		"node_id", syntheticID, "os_project_id", osProject.ID,
		"project_name", osProject.Name, "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if err := r.store.UpsertNode(ctx, leaf); err != nil {
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
		baseName, _ := strings.CutPrefix(token, "group:")
		// Groups have no scope parent and carry no tags, so the prefix is what
		// marks them as ours and keeps them out of the way of foreign groups.
		osGroupName := r.cfg.GroupPrefix + baseName

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
	if !r.cfg.NoDelete {
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
	}

	res.groupsSynced++
}

// syncGroupAssignments reconciles the Keystone group role assignments for a
// single project based on the group: tokens in the leaf's AuthorizedUsers.
// Non-fatal: errors are logged and skipped.
func (r *Reconciler) syncGroupAssignments(leaf tree.Node, osProjectID string, groupTokenToOSID map[string]string) {
	if r.osClient == nil || r.cfg.DryRun || len(groupTokenToOSID) == 0 {
		if r.cfg.DryRun {
			r.log.Debugw("Dry run: skipping group assignment sync",
				"node_id", leaf.ID, "os_project_id", osProjectID)
		}
		return
	}

	// Build desired group assignments for this leaf.
	type desired struct{ groupID, roleName string }
	var desiredList []desired
	for _, au := range leaf.AuthorizedUsers {
		if id, ok := groupTokenToOSID[au.Token]; ok {
			desiredList = append(desiredList, desired{groupID: id, roleName: au.OpenstackRole})
		}
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
	if !r.cfg.NoDelete {
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
}
