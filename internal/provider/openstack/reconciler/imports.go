package reconciler

import (
	"context"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

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
	existingImported []model.Node,
	osProjectByOSID map[string]osclient.ProjectInfo,
	importedByOSProjectID map[string]model.Node,
	osProjectByResourceID map[string]osclient.ProjectInfo,
	res *reconcileResult,
) {
	for _, leaf := range existingImported {
		if !slices.Contains(leaf.Flags, model.FlagPromoteOnReconcile) {
			continue
		}

		osProject, ok := osProjectByOSID[leaf.ExternalID]
		if !ok {
			r.log.Warnw("Cannot promote: OS project not found in scope",
				"node_id", leaf.ID, "os_project_id", leaf.ExternalID)
			continue
		}

		r.log.Infow("Promoting imported leaf to managed project",
			"node_id", leaf.ID, "os_project_id", leaf.ExternalID, "dry_run", r.cfg.DryRun)

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
			// The promotion may have been withdrawn while this pass talked to
			// OpenStack; then there is nothing to promote any more.
			promoted, err := r.nodes.Promote(ctx, leaf.ID, current)
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
		delete(osProjectByOSID, leaf.ExternalID)
		delete(importedByOSProjectID, leaf.ExternalID)
		osProjectByResourceID[leaf.ID] = osProject

		res.projectsPromoted++
	}
}

// removeStaleImports deletes imported leaves whose OpenStack project is no longer
// in scope — each only if it is still an import when the delete happens.
func (r *Reconciler) removeStaleImports(ctx context.Context, importedByOSProjectID map[string]model.Node, osProjectByOSID map[string]osclient.ProjectInfo, res *reconcileResult) {
	for osID, staleLeaf := range importedByOSProjectID {
		if _, stillInScope := osProjectByOSID[osID]; stillInScope {
			continue
		}
		r.log.Infow("Removing stale imported leaf (OS project gone from scope)",
			"node_id", staleLeaf.ID, "os_project_id", osID)
		if !r.cfg.DryRun {
			deleted, err := r.nodes.RemoveImport(ctx, staleLeaf.ID)
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

// upsertImported creates or refreshes a synthetic imported leaf under the
// structural "unassigned" node. IDs are stable: if a leaf already exists for this
// OS project its ID is reused; otherwise a new "p_<uuid>" ID is generated.
func (r *Reconciler) upsertImported(
	ctx context.Context,
	osProject osclient.ProjectInfo,
	existing map[string]model.Node,
	grantUse osclient.GrantUse,
	res *reconcileResult,
) {
	syntheticID := "p_" + uuid.New().String()
	if prev, ok := existing[osProject.ID]; ok {
		syntheticID = prev.ID // keep the existing ID stable across reconcile runs
	}

	var osLimit common.ProjectQuota
	// What the import uses, from the same response as its limit; nil when that
	// could not be read, and then the last known values stay.
	var measured *tree.Observation
	detail, err := r.osClient.GetProjectQuotaDetail(osProject.ID)
	if err != nil {
		r.log.Warnw("Could not fetch quota for import",
			"os_project_id", osProject.ID, "error", err)
		osLimit = common.ProjectQuota{}
	} else {
		osLimit = QuotaSetToProjectQuota(r.resources(), detail.Limit)
		measured = measureImport(r.resources(), osLimit, detail)
		measured.GrantUse = projectGrantUse(r.resources(), grantUse, osProject.ID)
	}
	var prevLimit common.ProjectQuota
	if prev, ok := existing[osProject.ID]; ok {
		prevLimit = prev.Limit
	}
	importGrants(r.osClient, r.resources(), osProject.ID, osLimit, prevLimit, r.log)

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

	parent := model.UnassignedNodeID
	leaf := model.Node{
		ID:                       syntheticID,
		Kind:                     model.KindProject,
		ParentID:                 &parent,
		Status:                   model.StatusImported,
		Name:                     osProject.Name,
		Reason:                   fmt.Sprintf("OpenStack project: %s (%s)", osProject.Name, osProject.ID),
		Limit:                    osLimit,
		AuthorizedUsers:          authorizedUsers,
		ExternalGroupAssignments: externalGroups,
		History:                  []model.HistoryEntry{},
		CreatedBy:                "System",
		ExternalID:               osProject.ID,
		ExternalName:             osProject.Name,
	}
	tree.ApplyObservation(&leaf, osProject.ID, measured)

	r.log.Infow("Upserting imported leaf",
		"node_id", syntheticID, "os_project_id", osProject.ID,
		"project_name", osProject.Name, "dry_run", r.cfg.DryRun)

	_, known := existing[osProject.ID]
	if !known {
		leaf.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if !r.cfg.DryRun {
		if known {
			// Only what OpenStack says about the project; the rest of the node
			// belongs to the tree (see tree.ProviderSync.RefreshImport).
			if err := r.nodes.RefreshImport(ctx, leaf, measured); err != nil {
				r.log.Warnw("Failed to update imported leaf",
					"id", syntheticID, "error", err)
				return
			}
		} else if err := r.nodes.AddImport(ctx, leaf); err != nil {
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

// importGrants records the availabilities an imported project already has in
// OpenStack — a shared network, flavour or image of the catalogue — in its
// limit, the same way as its quota. Adopting the project starts from that
// limit; without them the adopt dialog offered every availability as off, and
// accepting it asked the reconciler to take, say, the IPv4 network away from
// servers still attached to it. A grant that cannot be read keeps the value of
// the last pass, so a passing error does not read as "not granted".
func importGrants(c grantClient, defs []osclient.ManagedResource, osProjectID string, limit, prev common.ProjectQuota, log *zap.SugaredLogger) {
	for _, def := range defs {
		if !def.IsBool() || def.Grant == nil {
			continue
		}
		has, err := c.HasGrant(*def.Grant, osProjectID)
		if err != nil {
			log.Warnw("Could not read availability for import",
				"os_project_id", osProjectID, "resource", def.ID, "error", err)
			if v, ok := prev[def.ID]; ok {
				limit[def.ID] = v
			}
			continue
		}
		if has {
			limit[def.ID] = 1
		} else {
			limit[def.ID] = 0
		}
	}
}

// measureImport is what an imported project uses, read like a managed one's.
// Its limit is its OpenStack quota, so overcommitted means OpenStack already
// holds more than the quota allows.
func measureImport(resources []osclient.ManagedResource, limit common.ProjectQuota, detail *osclient.ProjectQuotaDetail) *tree.Observation {
	return &tree.Observation{
		Overcommitted: IsProjectOvercommitted(resources, limit, detail),
		InUse:         ProjectInUse(resources, detail),
		Servers:       detail.InUse.Instances,
		ServerLimit:   detail.Limit.Instances,
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
