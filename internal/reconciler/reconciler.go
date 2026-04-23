// Package reconciler implements a two-way sync between the resource store and OpenStack projects.
//
// Direction 1 — Storage → OpenStack:
//
//	Approved (and change_pending) requests are projected as OpenStack projects.
//	The project is created on first encounter and its quota is kept in sync with the
//	approved ProjectQuota on every subsequent run. For change_pending requests the
//	current approved quota is used — the proposed change is only applied after a manager
//	approves it in the service layer.
//
// Direction 2 — OpenStack → Storage:
//
//	Projects that carry ManagedProjectTag but have no matching active request in storage
//	are imported as synthetic "openstack_only" Request records. These are read-only from
//	the API perspective and contribute to overall resource consumption reporting even when
//	they cause delegation limits to be exceeded.
//
//	If ScopeParentID is configured the reconciler additionally scans all projects under
//	that parent, treating any project without a dhbw-resource-id tag as openstack_only
//	(i.e. projects created directly in OpenStack without going through the management UI).
package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"go.uber.org/zap"
)

// ReconcilerStore is the minimal storage interface the reconciler requires.
// It is a subset of applogic.ProjectStore.
type ReconcilerStore interface {
	ListProjectsByStatus(ctx context.Context, statuses []string, limit, offset int) ([]common.Project, error)
	UpsertProject(ctx context.Context, proj common.Project) error
	DeleteProject(ctx context.Context, id string) error
}

// Config holds all tunables for the reconciler.
type Config struct {
	// Interval between automatic reconciliation runs. Default: 5 minutes.
	Interval time.Duration
	// ProjectPrefix is prepended to the project ID when naming new OS projects.
	// Example: "dhbw-" produces the project name "dhbw-proj_1234567890".
	ProjectPrefix string
	// ScopeParentID, when non-empty, makes the reconciler list ALL projects under this
	// OpenStack parent project and import unknown ones as openstack_only records.
	// When empty only projects tagged with ManagedProjectTag are considered.
	ScopeParentID string
	// DryRun prevents any writes to OpenStack or the store; useful for testing.
	DryRun bool

	// NoDelete prevents all destructive operations. When true:
	//   - Released OS projects are always tagged for pending deletion (never deleted), regardless of DeleteReleasedProjects.
	//   - Stale openstack_only store records are kept (not removed from the database).
	//   - Orphaned managed Keystone users have their description updated to OrphanedUserFlagDescription instead of being deleted.
	//   - Group membership removals and project member removals are skipped.
	//   - Group role un-assignments from projects are skipped.
	// This is intended as a "phase 1" safe mode while the reconciler is being introduced.
	NoDelete bool

	// DeleteReleasedProjects controls what happens to OS projects whose request is released.
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
	// ContactTagPrefix is the prefix for tags that record requester contact addresses.
	// Default: "contact:".
	ContactTagPrefix string
}

// Status is returned by GetStatus to report the outcome of the last reconciliation run.
type Status struct {
	LastRunAt                 time.Time `json:"last_run_at"`
	LastError                 string    `json:"last_error,omitempty"`
	ProjectsSynced            int       `json:"projects_synced"`
	ProjectsCreated           int       `json:"projects_created"`
	OSOnlyImported            int       `json:"os_only_imported"`
	OSOnlyRemoved             int       `json:"os_only_removed"`
	OrphanedUsersRemoved      int       `json:"orphaned_users_removed"`
	GroupsCreated             int       `json:"groups_created"`
	GroupsSynced              int       `json:"groups_synced"`
	ProjectsTaggedForDeletion int       `json:"projects_tagged_for_deletion"`
	ProjectsDeleted           int       `json:"projects_deleted"`
	ProjectsPromoted          int       `json:"projects_promoted"`
	Running                   bool      `json:"running"`
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
	if cfg.ProjectPrefix == "" {
		cfg.ProjectPrefix = "managed-"
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

func (r *Reconciler) runOnce(ctx context.Context) {
	r.mu.Lock()
	r.status.Running = true
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
		r.status.OSOnlyImported = result.osOnlyImported
		r.status.OSOnlyRemoved = result.osOnlyRemoved
		r.status.OrphanedUsersRemoved = result.orphanedUsersRemoved
		r.status.GroupsCreated = result.groupsCreated
		r.status.GroupsSynced = result.groupsSynced
		r.status.ProjectsTaggedForDeletion = result.projectsTaggedForDeletion
		r.status.ProjectsDeleted = result.projectsDeleted
		r.status.ProjectsPromoted = result.projectsPromoted
		r.log.Infow("Reconciliation complete",
			"synced", result.projectsSynced,
			"created", result.projectsCreated,
			"os_only_imported", result.osOnlyImported,
			"os_only_removed", result.osOnlyRemoved,
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
	osOnlyImported            int
	osOnlyRemoved             int
	orphanedUsersRemoved      int
	groupsCreated             int
	groupsSynced              int
	projectsTaggedForDeletion int
	projectsDeleted           int
	projectsPromoted          int
}

// Reconcile performs one full two-way sync and returns a summary.
// Safe to call directly without Start (e.g. from tests).
func (r *Reconciler) Reconcile(ctx context.Context) (reconcileResult, error) {
	var res reconcileResult

	// ── Phase 1: load state from both sides ──────────────────────────────────

	activeProjects, err := r.store.ListProjectsByStatus(ctx, common.ReconcilableProjectStatuses, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load active projects: %w", err)
	}

	// allKnownProjects covers every real status so Phase 5 can tell whether a tagged
	// OS project is already tracked (in any state) before deciding to import it as
	// openstack_only.  This prevents projects from being re-imported when their
	// request is e.g. pending, change_rejected, rejected, or released.
	allKnownProjects, err := r.store.ListProjectsByStatus(ctx, common.KnownProjectStatuses, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load all known projects: %w", err)
	}

	// releasedProjectByID is a subset of allKnownProjects used in Phase 5 to tag or
	// delete OS projects whose project has been released.
	releasedProjectByID := make(map[string]common.Project, len(allKnownProjects))
	for _, proj := range allKnownProjects {
		if proj.Status == common.ProjectStatusReleased {
			releasedProjectByID[proj.ID] = proj
		}
	}

	existingOSOnly, err := r.store.ListProjectsByStatus(ctx, []string{common.ProjectStatusOpenStackOnly}, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load openstack_only records: %w", err)
	}

	osProjects, err := r.loadScopedOSProjects()
	if err != nil {
		return res, fmt.Errorf("list OS projects: %w", err)
	}

	// ── Phase 2: build lookup maps ────────────────────────────────────────────

	projectByID := make(map[string]common.Project, len(activeProjects))
	for _, proj := range activeProjects {
		projectByID[proj.ID] = proj
	}

	// knownProjectIDs covers all real statuses; used in Phase 5 to avoid re-importing
	// a tagged OS project whose project exists but is not in a reconcilable state.
	knownProjectIDs := make(map[string]struct{}, len(allKnownProjects))
	for _, proj := range allKnownProjects {
		knownProjectIDs[proj.ID] = struct{}{}
	}

	// osProjectByResourceID: tagged OS projects keyed by their embedded project ID.
	osProjectByResourceID := make(map[string]osclient.ProjectInfo, len(osProjects))
	// osProjectByOSID: all scoped OS projects keyed by their OS project ID.
	osProjectByOSID := make(map[string]osclient.ProjectInfo, len(osProjects))
	for _, p := range osProjects {
		osProjectByOSID[p.ID] = p
		if rid := r.osClient.ExtractResourceIDFromTags(p.Tags); rid != "" {
			osProjectByResourceID[rid] = p
		}
	}

	// osOnlyByOSProjectID: existing openstack_only store records keyed by their OSProjectID.
	osOnlyByOSProjectID := make(map[string]common.Project, len(existingOSOnly))
	for _, req := range existingOSOnly {
		if req.OSProjectID != "" {
			osOnlyByOSProjectID[req.OSProjectID] = req
		}
	}

	// ── Phase 2.5: Promote openstack_only records flagged for promotion ──────
	//
	// Must run after the lookup maps are built (Phase 2) but before Phase 3/4 so
	// that the newly-tagged OS project is not re-imported as openstack_only in Phase 5.
	// Promoted entries are removed from osProjectByOSID and osOnlyByOSProjectID so
	// the Phase 5 loops never see them.
	r.promoteOSOnlyProjects(ctx, existingOSOnly, osProjectByOSID, osOnlyByOSProjectID, osProjectByResourceID, &res)

	// ── Phase 3: Sync OS groups and their memberships ───────────────────────
	// Collect every group: token referenced across all active projects, then
	// ensure each maps to a real Keystone group and its members are up to date.

	// groupTokenToOSID maps a group token (e.g. "group:dept_cs_faculty") to the
	// corresponding Keystone group ID. Built here and reused in Phase 4 when
	// assigning groups to projects.
	groupTokenToOSID := r.syncGroups(ctx, activeProjects, &res)

	// ── Phase 4: Storage → OpenStack (project create / quota sync) ───────────

	for _, proj := range activeProjects {
		osProject, hasProject := osProjectByResourceID[proj.ID]
		if !hasProject {
			created, err := r.createOpenstackProjectForRequest(ctx, proj)
			if err != nil {
				r.log.Warnw("Failed to create OS project for project", "project_id", proj.ID, "error", err)
				continue
			}
			res.projectsCreated++
			proj.OSProjectID = created.ID
			if !r.cfg.DryRun {
				if err := r.store.UpsertProject(ctx, proj); err != nil {
					r.log.Warnw("Failed to persist OSProjectID on project", "project_id", proj.ID, "error", err)
				}
			}
			r.syncMembers(proj, created.ID)
			r.syncGroupAssignments(proj, created.ID, groupTokenToOSID)
		} else {
			overcommitted, err := r.syncQuota(proj, osProject.ID)
			if err != nil {
				r.log.Warnw("Failed to sync quota for project", "project_id", proj.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
			if proj.OSProjectID != osProject.ID || proj.OSOvercommitted != overcommitted {
				proj.OSProjectID = osProject.ID
				proj.OSOvercommitted = overcommitted
				if !r.cfg.DryRun {
					if err := r.store.UpsertProject(ctx, proj); err != nil {
						r.log.Warnw("Failed to persist OS sync state on project", "project_id", proj.ID, "error", err)
					}
				}
			}
			r.syncMembers(proj, osProject.ID)
			r.syncGroupAssignments(proj, osProject.ID, groupTokenToOSID)
			res.projectsSynced++
		}
	}

	// ── Phase 5: OpenStack → Storage (import / remove openstack_only records) ─

	for osID, osProject := range osProjectByOSID {
		resourceID := r.osClient.ExtractResourceIDFromTags(osProject.Tags)
		if resourceID != "" {
			if _, active := projectByID[resourceID]; active {
				continue // managed + active → handled in phase 4
			}
			if releasedProj, wasReleased := releasedProjectByID[resourceID]; wasReleased {
				// Project was released: tag the project for pending deletion or delete it.
				r.handleReleasedProject(osProject, releasedProj, &res)
				delete(osOnlyByOSProjectID, osID)
				continue
			}
			if _, known := knownProjectIDs[resourceID]; known {
				continue // project exists in a non-reconcilable state (pending/rejected/…); skip
			}
			// Resource ID points to a project that no longer exists in storage at all
			// (e.g. hard-deleted) — treat as orphaned and import as openstack_only.
		}
		// Either untagged (externally created) or orphaned — treat as openstack_only.
		r.upsertOSOnly(ctx, osProject, osOnlyByOSProjectID, &res)
		delete(osOnlyByOSProjectID, osID) // mark as seen so we don't remove it below
	}

	// Clean up openstack_only store records whose OS projects no longer exist.
	for osID, staleRecord := range osOnlyByOSProjectID {
		if _, stillExists := osProjectByOSID[osID]; !stillExists {
			if r.cfg.NoDelete {
				r.log.Infow("NoDelete: skipping removal of stale openstack_only record (OS project gone)",
					"record_id", staleRecord.ID, "os_project_id", osID)
				continue
			}
			r.log.Infow("Removing stale openstack_only record (OS project gone)",
				"record_id", staleRecord.ID, "os_project_id", osID)
			if !r.cfg.DryRun {
				if err := r.store.DeleteProject(ctx, staleRecord.ID); err != nil {
					r.log.Warnw("Failed to delete stale openstack_only record",
						"id", staleRecord.ID, "error", err)
				}
			}
			res.osOnlyRemoved++
		}
	}

	// ── Phase 5: Remove auto-created Keystone users with no project memberships ─
	//
	// Users are pre-created by FindOrCreateUser when a project is approved. Once
	// a project is released/rejected and all project memberships are removed, the
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

// promoteOSOnlyProjects processes openstack_only records that carry the
// ProjectFlagPromoteOnReconcile flag. For each:
//  1. The OS project is tagged with the managed marker and the record's resource ID.
//  2. The record's status is changed to "pending" and the flag is removed.
//  3. The entry is removed from the Phase-5 lookup maps so it is not re-imported.
//
// After this phase the record flows through the normal pending → approved cycle.
// Non-fatal: failures for individual records are logged and skipped.
func (r *Reconciler) promoteOSOnlyProjects(
	ctx context.Context,
	existingOSOnly []common.Project,
	osProjectByOSID map[string]osclient.ProjectInfo,
	osOnlyByOSProjectID map[string]common.Project,
	osProjectByResourceID map[string]osclient.ProjectInfo,
	res *reconcileResult,
) {
	for _, record := range existingOSOnly {
		if !slices.Contains(record.Flags, common.ProjectFlagPromoteOnReconcile) {
			continue
		}

		osProject, ok := osProjectByOSID[record.OSProjectID]
		if !ok {
			r.log.Warnw("Cannot promote: OS project not found in scope",
				"record_id", record.ID, "os_project_id", record.OSProjectID)
			continue
		}

		r.log.Infow("Promoting openstack_only record to managed project",
			"record_id", record.ID, "os_project_id", record.OSProjectID, "dry_run", r.cfg.DryRun)

		if !r.cfg.DryRun {
			if err := r.osClient.TagProjectForPromotion(osProject.ID, record.ID, osProject.Tags); err != nil {
				r.log.Warnw("Failed to tag OS project for promotion",
					"record_id", record.ID, "os_project_id", osProject.ID, "error", err)
				continue
			}
		}

		promoted := record
		promoted.Status = common.ProjectStatusPending
		promoted.Flags = removeFlag(record.Flags, common.ProjectFlagPromoteOnReconcile)

		if !r.cfg.DryRun {
			if err := r.store.UpsertProject(ctx, promoted); err != nil {
				r.log.Warnw("Failed to persist promoted project",
					"record_id", record.ID, "error", err)
				continue
			}
		}

		// Remove from Phase-5 maps so the newly-tagged OS project is not re-imported.
		delete(osProjectByOSID, record.OSProjectID)
		delete(osOnlyByOSProjectID, record.OSProjectID)
		osProjectByResourceID[record.ID] = osProject

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

// handleReleasedProject either deletes or tags an OS project whose project has been
// released, depending on Config.DeleteReleasedProjects.
//
// When deletion is disabled (default) the project receives:
//   - a pending-deletion date tag (<PendingDeletionTagPrefix><YYYY-MM-DD>)
//   - one contact tag per requester email (<ContactTagPrefix><email>)
//
// The tagging is idempotent: if the pending-deletion tag is already present the
// project is left unchanged on subsequent reconcile runs.
func (r *Reconciler) handleReleasedProject(osProject osclient.ProjectInfo, proj common.Project, res *reconcileResult) {
	if r.cfg.DeleteReleasedProjects && !r.cfg.NoDelete {
		r.log.Infow("Deleting OS project for released project",
			"os_project_id", osProject.ID, "project_id", proj.ID, "dry_run", r.cfg.DryRun)
		if !r.cfg.DryRun {
			if err := r.osClient.DeleteProject(osProject.ID); err != nil {
				r.log.Warnw("Failed to delete OS project for released project",
					"os_project_id", osProject.ID, "project_id", proj.ID, "error", err)
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
	// the new pending-deletion date tag and one contact tag per requester email.
	newTags := make([]string, 0, len(osProject.Tags)+4)
	for _, tag := range osProject.Tags {
		if !strings.HasPrefix(tag, r.cfg.ContactTagPrefix) {
			newTags = append(newTags, tag)
		}
	}
	newTags = append(newTags, r.cfg.PendingDeletionTagPrefix+deletionDate)
	for _, token := range proj.RequesterTokens {
		if email, ok := strings.CutPrefix(token, "user:"); ok {
			newTags = append(newTags, r.cfg.ContactTagPrefix+email)
		}
	}

	r.log.Infow("Tagging OS project for pending deletion",
		"os_project_id", osProject.ID, "project_id", proj.ID,
		"deletion_date", deletionDate, "contacts", len(proj.RequesterTokens), "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if _, err := r.osClient.UpdateProject(osProject.ID, osclient.ProjectUpdateOpts{
			Tags: &newTags,
		}); err != nil {
			r.log.Warnw("Failed to tag OS project for pending deletion",
				"os_project_id", osProject.ID, "project_id", proj.ID, "error", err)
			return
		}
	}
	res.projectsTaggedForDeletion++
}

// loadScopedOSProjects fetches the OS projects to reconcile against.
// When ScopeParentID is set it lists all children of that parent so externally created
// projects can be imported as openstack_only. Otherwise only managed-tagged projects.
func (r *Reconciler) loadScopedOSProjects() ([]osclient.ProjectInfo, error) {
	if r.cfg.ScopeParentID != "" {
		return r.osClient.CollectProjectsByParent(r.cfg.ScopeParentID)
	}
	return r.osClient.CollectManagedProjects()
}

// buildDescription constructs the OS project description for a project.
// Format: "email: reason" where email is the first requester's address.
func buildDescription(proj common.Project) string {
	email := ""
	for _, token := range proj.RequesterTokens {
		if e, ok := strings.CutPrefix(token, "user:"); ok {
			email = e
			break
		}
	}
	if email != "" && proj.Reason != "" {
		return email + ": " + proj.Reason
	}
	if proj.Reason != "" {
		return proj.Reason
	}
	return fmt.Sprintf("Managed by DHBW resource management. Project: %s", proj.ID)
}

// createProjectForRequest creates a new OpenStack project for an approved project and
// applies the full initial quota (managed fields + network defaults).
// On quota-set failure the orphan project is deleted before returning the error.
func (r *Reconciler) createOpenstackProjectForRequest(_ context.Context, proj common.Project) (osclient.ProjectInfo, error) {
	name := r.cfg.ProjectPrefix + proj.ID
	description := buildDescription(proj)

	r.log.Infow("Creating OS project for project",
		"project_id", proj.ID, "project_name", name, "dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return osclient.ProjectInfo{ID: "dry-run-" + proj.ID, Name: name}, nil
	}

	project, err := r.osClient.CreateManagedProject(name, description, r.cfg.ScopeParentID, proj.ID)
	if err != nil {
		return osclient.ProjectInfo{}, fmt.Errorf("create project: %w", err)
	}

	// Compose a full quota set: managed resources from the proj + static defaults.
	// Static defaults (network quotas, volumes, snapshots) are driven entirely by the
	// ManagedProject definitions — no separate DefaultNetworkQuotas struct needed.
	fullQuota := ProjectQuotaToQuotaSet(r.managedProjects, proj.Quota)
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
			"project_id", project.ID, "project_id", proj.ID, "error", quotaErr)
		// Return the project so the caller persists OSProjectID. The next reconcile cycle
		// will find the project via its tag and call syncQuota, which retries quota updates.
	}

	r.log.Infow("OS project created and quota set",
		"project_id", proj.ID, "project_id", project.ID)
	return osclient.ProjectInfo{ID: project.ID, Name: project.Name, Tags: project.Tags}, nil
}

// syncQuota pushes the current approved quota to an existing OS project and returns
// whether the project is currently overcommitted (in-use > new limit).
// For change_pending projects the current approved quota (proj.Quota) is used —
// the proposed pending change only takes effect after manager approval.
func (r *Reconciler) syncQuota(proj common.Project, osProjectID string) (overcommitted bool, err error) {
	quotaSet := ProjectQuotaToQuotaSet(r.managedProjects, proj.Quota)

	r.log.Debugw("Syncing managed quota",
		"project_id", proj.ID, "os_project_id", osProjectID,
		"cores", quotaSet.Cores, "ram_mb", quotaSet.RAM, "gigabytes", quotaSet.Gigabytes,
		"dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return false, nil
	}

	if err := r.osClient.UpdateManagedQuotas(osProjectID, quotaSet); err != nil {
		return false, fmt.Errorf("update managed quotas: %w", err)
	}

	description := buildDescription(proj)
	if _, err := r.osClient.UpdateProject(osProjectID, osclient.ProjectUpdateOpts{
		BaseProjectOpts: osclient.BaseProjectOpts{Description: &description},
	}); err != nil {
		r.log.Warnw("Failed to update OS project description",
			"project_id", proj.ID, "os_project_id", osProjectID, "error", err)
	}

	// Overcommit check: OpenStack accepts a quota reduction below current usage but blocks
	// new resource creation. We surface this in the UI via the OSOvercommitted flag.
	detail, err := r.osClient.GetProjectQuotaDetail(osProjectID)
	if err != nil {
		r.log.Warnw("Skipping overcommit check (quota detail unavailable)",
			"project_id", proj.ID, "os_project_id", osProjectID, "error", err)
		return false, nil
	}

	return IsProjectOvercommitted(r.managedProjects, proj.Quota, detail), nil
}

// buildDesiredMembers extracts the intended OpenStack role assignments from a request.
// Only user: tokens are processed — group: tokens have no direct Keystone equivalent.
// RequesterTokens → admin role; AuthorizedUsers → their specified OpenStack role.
func buildDesiredMembers(proj common.Project) []osclient.DesiredMember {
	desired := make([]osclient.DesiredMember, 0, len(proj.RequesterTokens)+len(proj.AuthorizedUsers))
	for _, token := range proj.RequesterTokens {
		if email, ok := strings.CutPrefix(token, "user:"); ok {
			desired = append(desired, osclient.DesiredMember{
				Email:    email,
				RoleName: "admin",
			})
		}
	}
	for _, au := range proj.AuthorizedUsers {
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
// project's RequesterTokens (admin) and AuthorizedUsers. Non-fatal: errors are logged
// but do not interrupt the reconciliation run.
func (r *Reconciler) syncMembers(proj common.Project, osProjectID string) {
	if r.cfg.DryRun {
		r.log.Debugw("Dry run: skipping member sync",
			"project_id", proj.ID, "os_project_id", osProjectID)
		return
	}
	desired := buildDesiredMembers(proj)
	var memberSyncErr error
	if r.cfg.NoDelete {
		memberSyncErr = r.osClient.EnsureProjectMembers(osProjectID, desired)
	} else {
		memberSyncErr = r.osClient.SyncProjectMembers(osProjectID, desired)
	}
	if memberSyncErr != nil {
		r.log.Warnw("Member sync failed",
			"project_id", proj.ID, "os_project_id", osProjectID, "error", memberSyncErr)
	}
}

// upsertOSOnly creates or refreshes a synthetic openstack_only record in storage.
// IDs are stable: if a record already exists for this OS project its ID is reused;
// otherwise a new "req_<uuid>" ID is generated (the old "osonly-" prefix is no longer used).
func (r *Reconciler) upsertOSOnly(
	ctx context.Context,
	osProject osclient.ProjectInfo,
	existing map[string]common.Project,
	res *reconcileResult,
) {
	syntheticID := "req_" + uuid.New().String()
	if prev, ok := existing[osProject.ID]; ok {
		syntheticID = prev.ID // keep the existing ID stable across reconcile runs
	}

	var osQuota common.ProjectQuota
	detail, err := r.osClient.GetProjectQuotaDetail(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch quota for openstack_only import",
			"os_project_id", osProject.ID, "error", err)
		osQuota = common.ProjectQuota{}
	} else {
		osQuota = QuotaSetToProjectQuota(r.managedProjects, detail.Limit)
	}

	// Resolve project members: admin-role users become RequesterTokens (they see the project
	// in "My Resources"), non-admin users become AuthorizedUsers.
	requesterTokens := common.TokenList{}
	authorizedUsers := []common.AuthorizedUser{}
	members, err := r.osClient.ListProjectMemberInfo(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch member info for openstack_only import, tokens will be empty",
			"os_project_id", osProject.ID, "error", err)
	} else {
		for _, m := range members {
			token := "user:" + m.Email
			if m.RoleName == "admin" {
				requesterTokens = append(requesterTokens, token)
			} else {
				authorizedUsers = append(authorizedUsers, common.AuthorizedUser{
					Token:         token,
					OpenstackRole: m.RoleName,
				})
			}
		}
	}

	// Resolve group role assignments. Groups whose name resolves to a known group: token
	// are added to RequesterTokens / AuthorizedUsers so they appear in the promote modal.
	// Groups that cannot be resolved (external / non-managed) are stored separately so the
	// reconciler can preserve them without exposing them to the delegation management flow.
	var externalGroups []common.ExternalGroupAssignment
	groupRoles, err := r.osClient.ListProjectGroupRoles(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch group roles for openstack_only import",
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
			token := "group:" + osGroup.Name
			if g.RoleName == "admin" {
				requesterTokens = append(requesterTokens, token)
			} else {
				authorizedUsers = append(authorizedUsers, common.AuthorizedUser{
					Token:         token,
					OpenstackRole: g.RoleName,
				})
			}
		}
	}

	record := common.Project{
		ID:                       syntheticID,
		Status:                   common.ProjectStatusOpenStackOnly,
		RequesterTokens:          requesterTokens,
		Quota:                    osQuota,
		Reason:                   fmt.Sprintf("OpenStack project: %s (%s)", osProject.Name, osProject.ID),
		FundedBy:                 nil,
		AuthorizedUsers:          authorizedUsers,
		ExternalGroupAssignments: externalGroups,
		History:                  []common.HistoryEntry{},
		OSProjectID:              osProject.ID,
		OSProjectName:            osProject.Name,
	}

	// Preserve existing history and flags so they aren't wiped on every reconcile cycle.
	if prev, ok := existing[osProject.ID]; ok {
		record.History = prev.History
		record.Flags = prev.Flags
	}

	r.log.Infow("Upserting openstack_only record",
		"synthetic_id", syntheticID, "os_project_id", osProject.ID,
		"project_name", osProject.Name, "dry_run", r.cfg.DryRun)

	if !r.cfg.DryRun {
		if err := r.store.UpsertProject(ctx, record); err != nil {
			r.log.Warnw("Failed to upsert openstack_only record",
				"id", syntheticID, "error", err)
			return
		}
	}

	if _, wasKnown := existing[osProject.ID]; wasKnown {
		res.projectsSynced++
	} else {
		res.osOnlyImported++
	}
}

// collectGroupTokens returns the set of unique group: tokens referenced by any
// RequesterTokens or AuthorizedUsers entry across the given projects.
func collectGroupTokens(projects []common.Project) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, proj := range projects {
		for _, t := range proj.RequesterTokens {
			if strings.HasPrefix(t, "group:") {
				tokens[t] = struct{}{}
			}
		}
		for _, au := range proj.AuthorizedUsers {
			if strings.HasPrefix(au.Token, "group:") {
				tokens[au.Token] = struct{}{}
			}
		}
	}
	return tokens
}

// syncGroups ensures a Keystone group exists for every group: token referenced
// in active projects, populates each group's membership from the role provider,
// and returns a map of groupToken → Keystone group ID for use in project assignment.
// Non-fatal: errors for individual groups are logged but do not abort the run.
func (r *Reconciler) syncGroups(ctx context.Context, activeProjects []common.Project, res *reconcileResult) map[string]string {
	if r.osClient == nil {
		return nil
	}

	groupTokens := collectGroupTokens(activeProjects)
	groupTokenToOSID := make(map[string]string, len(groupTokens))

	for token := range groupTokens {
		baseName, _ := strings.CutPrefix(token, "group:")
		// Prefix the OS group name the same way projects are prefixed so all
		// managed resources share a consistent naming convention.
		osGroupName := r.cfg.ProjectPrefix + baseName

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
			r.log.Warnw("Could not find/create user for group membership",
				"group", groupName, "email", email, "error", err)
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
// single project based on the group: tokens in the project's RequesterTokens
// (admin role) and AuthorizedUsers. Non-fatal: errors are logged and skipped.
func (r *Reconciler) syncGroupAssignments(proj common.Project, osProjectID string, groupTokenToOSID map[string]string) {
	if r.osClient == nil || r.cfg.DryRun || len(groupTokenToOSID) == 0 {
		if r.cfg.DryRun {
			r.log.Debugw("Dry run: skipping group assignment sync",
				"project_id", proj.ID, "os_project_id", osProjectID)
		}
		return
	}

	// Build desired group assignments for this project.
	type desired struct{ groupID, roleName string }
	var desiredList []desired
	for _, t := range proj.RequesterTokens {
		if id, ok := groupTokenToOSID[t]; ok {
			desiredList = append(desiredList, desired{groupID: id, roleName: "admin"})
		}
	}
	for _, au := range proj.AuthorizedUsers {
		if id, ok := groupTokenToOSID[au.Token]; ok {
			desiredList = append(desiredList, desired{groupID: id, roleName: au.OpenstackRole})
		}
	}
	// External groups have no delegation token — add them by their OS group ID directly
	// so they are always preserved and never removed by the cleanup pass below.
	for _, eg := range proj.ExternalGroupAssignments {
		desiredList = append(desiredList, desired{groupID: eg.GroupID, roleName: eg.Role})
	}

	// Build desired set: groupID+roleName → true.
	desiredSet := make(map[string]string, len(desiredList)) // groupID → roleName
	for _, d := range desiredList {
		desiredSet[d.groupID] = d.roleName
	}

	// Fetch current group assignments for the project.
	current, err := r.osClient.ListProjectGroupRoles(osProjectID)
	if err != nil {
		r.log.Warnw("Could not list current group project roles, skipping group assignment sync",
			"project_id", proj.ID, "os_project_id", osProjectID, "error", err)
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
