package reconciler

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

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

// buildDesiredMembers extracts the intended OpenStack role assignments from a leaf.
// Only user: tokens are processed — group: tokens have no direct Keystone equivalent.
// The owner receives common.OwnerOpenstackRole; AuthorizedUsers their specified one.
func buildDesiredMembers(leaf model.Node) []osclient.DesiredMember {
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
func (r *Reconciler) syncMembers(leaf model.Node, osProjectID string) {
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

// relationSeparator stands in for "#" in Keystone group names:
// group:wwi23seb#dozent becomes "<prefix>wwi23seb--dozent". A group ID that
// itself ends in "--<relation>" would read back as a relation; relation names
// are lowercase words, so that takes a deliberately odd group name.
const relationSeparator = "--"

// keystoneGroupName is the Keystone group a group token is provisioned as.
// Plain groups keep the name they always had, so no existing group is renamed.
func keystoneGroupName(prefix, token string) string {
	group, relation := roles.SplitGroupToken(token)
	name := prefix + strings.TrimPrefix(group, common.GroupPrefix)
	if relation != roles.RelationMember {
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
			return roles.JoinGroupToken(common.GroupPrefix+base[:i], relation)
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
func collectGroupTokens(leaves []model.Node) map[string]struct{} {
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
func (r *Reconciler) syncGroups(ctx context.Context, activeLeaves []model.Node, res *reconcileResult) map[string]string {
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
func (r *Reconciler) syncGroupAssignments(leaf model.Node, osProjectID string, groupTokenToOSID map[string]string) {
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
	for _, eg := range leaf.Binding(osclient.Provider).ExternalGroupAssignments {
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
