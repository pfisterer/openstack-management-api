package reconciler

import (
	"context"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// tagReader is the narrow OpenStack surface chooseRecoverableProject needs.
// *osclient.OpenStackClient satisfies it; tests substitute a fake.
type tagReader interface {
	ExtractResourceIDFromTags(tags []string) string
}

// chooseRecoverableProject decides whether a leaf may reclaim the OpenStack
// project recorded on it. Separated from the writing half so the three ways this
// can go wrong are testable without a cloud.
func chooseRecoverableProject(
	c tagReader,
	leaf model.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	claimed map[string]string,
	log *zap.SugaredLogger,
) (osclient.ProjectInfo, bool) {
	if leaf.ExternalID == "" {
		return osclient.ProjectInfo{}, false // never had one — Phase 4 creates it
	}
	osProject, exists := osProjectByOSID[leaf.ExternalID]
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
// import, VMs and all. The node's stored ExternalID is the second witness
// that survives whatever happens to the tags, so it decides.
//
// The project is claimed even when re-tagging fails: a duplicate project is far
// worse than a tag that is restored one tick later, and every following tick
// retries. Returns the project and whether the leaf may use it.
func (r *Reconciler) recoverUntaggedProject(
	ctx context.Context,
	leaf model.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	importedByOSProjectID map[string]model.Node,
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
			deleted, err := r.nodes.RemoveImport(ctx, shadow.ID)
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
