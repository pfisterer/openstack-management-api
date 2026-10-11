package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

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
	released map[string]model.Node,
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
		if _, inScope := osProjectByOSID[osID(leaf)]; inScope && osID(leaf) != "" {
			continue
		}
		// No OpenStack project, but another provider still carries it out:
		// nothing to do here; that provider removes the leaf when it is done.
		if osID(leaf) == "" && r.nodes.BoundElsewhere(leaf) {
			continue
		}

		// Nothing is destroyed in OpenStack here — the project is already gone
		// from it, and keeping the record would leave the tree disagreeing with
		// the cloud.
		r.log.Infow("Removing released leaf — its OpenStack project is gone",
			"node_id", leaf.ID, "name", leaf.Name, "os_project_id", osID(leaf),
			"dry_run", r.cfg.DryRun)
		if !r.cfg.DryRun {
			deleted, err := r.nodes.RemoveRetired(ctx, leaf.ID)
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
func (r *Reconciler) handleReleasedProject(ctx context.Context, osProject osclient.ProjectInfo, leaf model.Node, scopeParentID string, res *reconcileResult) {
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
	if archived && leaf.Status == model.StatusReleased {
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
func (r *Reconciler) markLeafArchived(ctx context.Context, leaf model.Node) {
	if r.cfg.DryRun {
		r.log.Infow("Dry run: would mark leaf archived", "node_id", leaf.ID)
		return
	}
	if err := r.nodes.MarkArchived(ctx, leaf.ID); err != nil {
		r.log.Warnw("Failed to mark leaf archived", "node_id", leaf.ID, "error", err)
	}
}

// tagReleasedProject writes the pending-deletion, contact and status tags.
// Idempotent: the deletion day is set once and never moved — re-dating it on
// every pass would push it out for ever.
func (r *Reconciler) tagReleasedProject(osProject osclient.ProjectInfo, leaf model.Node, res *reconcileResult) []string {
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
