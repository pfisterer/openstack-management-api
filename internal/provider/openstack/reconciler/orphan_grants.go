package reconciler

import (
	"slices"

	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// orphanClient is the part of OpenStack the orphan sweep needs; tests substitute a fake.
type orphanClient interface {
	ListNetworkShares(networkID string) ([]osclient.NetworkShare, error)
	DeleteNetworkShare(policyID string) error
	ProjectExists(projectID string) (bool, error)
}

// sweepOrphanedGrants runs sweepOrphanedGrants against OpenStack, for the policies owned by the project the network client works from and by the scope parent.
func (r *Reconciler) sweepOrphanedGrants(known map[string]osclient.ProjectInfo, res *reconcileResult) {
	var owners []string
	for _, o := range []string{r.osClient.GrantOwnerProject(), r.scopeParentID} {
		if o != "" && !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
	}
	res.orphanedGrantsRemoved += sweepOrphanedGrants(r.osClient, r.resources(), owners, known, r.cfg.DryRun, r.log)
}

// sweepOrphanedGrants deletes the network shares (RBAC policies) of the catalogue whose project no longer exists. Deleting a project never removed them, and they count against the rbac_policy quota of the project that owns them: on 19.10.2026 half of that quota was taken by projects long gone, and every new grant failed with OverQuota. Deleting a project now revokes its grants (revokeGrants); this catches what was left before, what that failed to remove and what goes when a project is deleted by hand.
//
// Strict on purpose, since a wrong deletion takes a network away from someone:
//   - only the networks of the catalogue, never another object;
//   - only access_as_shared policies owned by one of owners, the projects the reconciler creates them from — a share an operator made from elsewhere is not looked at;
//   - only a concrete target: never "*" (shared with everyone), never empty, never an owner itself;
//   - only when Keystone answers 404 for the target: a refusal or any other error keeps the policy, and a project in the current listing (known) is not even asked about.
//
// Image members and flavour access are not swept: they carry no owner, so there is no telling the reconciler's from an operator's, and they count against no quota. Returns how many policies were deleted (or would be, in a dry run).
func sweepOrphanedGrants(c orphanClient, defs []osclient.ManagedResource, owners []string, known map[string]osclient.ProjectInfo, dryRun bool, log *zap.SugaredLogger) int {
	if len(owners) == 0 {
		log.Debugw("Orphan sweep skipped: no owner project known for the network shares")
		return 0
	}
	exists := map[string]bool{}
	removed := 0
	for _, def := range defs {
		if !def.IsBool() || def.Grant == nil || def.Grant.Type != osclient.GrantNetwork || def.Grant.Target == "" {
			continue
		}
		shares, err := c.ListNetworkShares(def.Grant.Target)
		if err != nil {
			log.Warnw("Orphan sweep: could not list the shares of a network", "resource", def.ID, "network_id", def.Grant.Target, "error", err)
			continue
		}
		for _, s := range shares {
			if s.NetworkID != def.Grant.Target || !slices.Contains(owners, s.OwnerProject) {
				continue
			}
			target := s.TargetProject
			if target == "" || target == "*" || slices.Contains(owners, target) {
				continue
			}
			if _, inListing := known[target]; inListing {
				continue
			}
			alive, checked := exists[target]
			if !checked {
				ok, err := c.ProjectExists(target)
				if err != nil {
					log.Warnw("Orphan sweep: could not tell whether the project exists, its share stays", "resource", def.ID, "policy_id", s.PolicyID, "target_project_id", target, "error", err)
					continue
				}
				alive = ok
				exists[target] = ok
			}
			if alive {
				continue
			}
			if dryRun {
				log.Infow("Dry run: would delete the share of a network with a project that no longer exists", "resource", def.ID, "network_id", s.NetworkID, "policy_id", s.PolicyID, "target_project_id", target, "owner_project_id", s.OwnerProject)
				removed++
				continue
			}
			if err := c.DeleteNetworkShare(s.PolicyID); err != nil {
				log.Warnw("Orphan sweep: could not delete the share of a network with a project that no longer exists", "resource", def.ID, "policy_id", s.PolicyID, "target_project_id", target, "error", err)
				continue
			}
			removed++
			log.Infow("Deleted the share of a network with a project that no longer exists", "resource", def.ID, "network_id", s.NetworkID, "policy_id", s.PolicyID, "target_project_id", target, "owner_project_id", s.OwnerProject)
		}
	}
	return removed
}
