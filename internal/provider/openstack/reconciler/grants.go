package reconciler

import (
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// CheckGrantTarget asks OpenStack whether g can be granted per project (for
// root admins adding an availability).
func (r *Reconciler) CheckGrantTarget(g osclient.Grant) error {
	parent := ""
	if p := r.scopeParentSeen.Load(); p != nil {
		parent = *p
	}
	return r.osClient.CheckGrantTarget(g, parent)
}

// GrantedProjects lists the projects OpenStack grants g to.
func (r *Reconciler) GrantedProjects(g osclient.Grant) ([]string, error) {
	return r.osClient.GrantedProjects(g)
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
	HasGrant(grant osclient.Grant, projectID string) (bool, error)
	// Both report whether they CHANGED anything, so a pass that alters access in
	// OpenStack leaves a line in the log and the twenty that do not stay quiet.
	AddGrant(grant osclient.Grant, projectID string) (bool, error)
	RemoveGrant(grant osclient.Grant, projectID string) (bool, error)
}

func (r *Reconciler) syncGrants(leaf model.Node, osProjectID string) {
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
func syncGrants(c grantClient, defs []osclient.ManagedResource, leaf model.Node, osProjectID string, dryRun bool, log *zap.SugaredLogger) {
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

// revokeGrants takes every availability of the catalogue away from a project that was just deleted. Keystone deletes the project and nothing else: a Neutron RBAC policy sharing a network with it stays behind, owned by the project the reconciler works from and counted against that project's rbac_policy quota, and once enough of them pile up every new grant fails with OverQuota. Image members and flavour access are left behind the same way, harmless but stale.
//
// Only the catalogue's targets, like syncGrants, and for a project the reconciler itself deleted, so there is nobody whose access this could take away. A failure is logged and left to the orphan sweep (sweepOrphanedGrants) on a later pass. Returns how many grants were removed.
func revokeGrants(c grantClient, defs []osclient.ManagedResource, osProjectID string, dryRun bool, log *zap.SugaredLogger) int {
	if osProjectID == "" {
		return 0
	}
	removed := 0
	for _, def := range defs {
		if !def.IsBool() || def.Grant == nil {
			continue
		}
		if dryRun {
			log.Infow("Dry run: would remove the availability of the deleted project",
				"os_project_id", osProjectID, "resource", def.ID, "grant_type", def.Grant.Type, "target", def.Grant.Target)
			continue
		}
		changed, err := c.RemoveGrant(*def.Grant, osProjectID)
		if err != nil {
			log.Warnw("Could not remove the availability of the deleted project; the orphan sweep retries",
				"os_project_id", osProjectID, "resource", def.ID, "error", err)
			continue
		}
		if changed {
			removed++
			log.Infow("Removed availability of the deleted project",
				"os_project_id", osProjectID, "resource", def.ID, "grant_type", def.Grant.Type, "target", def.Grant.Target)
		}
	}
	return removed
}

// readGrantUse reads what uses the availabilities of the catalogue, for all
// projects; nil when OpenStack could not be read, so no project's last known
// values are overwritten with "nothing".
func (r *Reconciler) readGrantUse() osclient.GrantUse {
	var flavors, images, networks []string
	for _, def := range r.resources() {
		if !def.IsBool() || def.Grant == nil {
			continue
		}
		switch def.Grant.Type {
		case osclient.GrantFlavor:
			flavors = append(flavors, def.Grant.Target)
		case osclient.GrantImage:
			images = append(images, def.Grant.Target)
		case osclient.GrantNetwork:
			networks = append(networks, def.Grant.Target)
		}
	}
	if len(flavors)+len(images)+len(networks) == 0 {
		return nil
	}
	use, err := r.osClient.ReadGrantUse(flavors, images, networks)
	if err != nil {
		r.log.Warnw("Could not read what uses the availabilities; keeping the last known values", "error", err)
		return nil
	}
	return use
}

// projectGrantUse is one project's share of use, keyed by catalogue resource:
// every availability appears, with 0 for one nothing uses. nil when use could
// not be read at all.
func projectGrantUse(defs []osclient.ManagedResource, use osclient.GrantUse, osProjectID string) common.ProjectQuota {
	if use == nil {
		return nil
	}
	out := common.ProjectQuota{}
	for _, def := range defs {
		if def.IsBool() && def.Grant != nil {
			out[def.ID] = use[osProjectID][def.Grant.Target]
		}
	}
	return out
}
