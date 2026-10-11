package osclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/pagination"
)

// ProjectRole represents a user's role assignment in a project
type ProjectRole struct {
	UserID    string
	ProjectID string
	RoleID    string
	RoleName  string
}

// ProjectMemberInfo carries a resolved user email and their role name in a project.
// Only users whose email can be determined are included; service accounts are skipped.
type ProjectMemberInfo struct {
	Email    string
	RoleName string
}

// DesiredMember describes the intended role assignment for one user in a project.
type DesiredMember struct {
	Email    string
	RoleName string
	// PrimaryProject marks the member for whom this project is their own — the
	// owner. Their Keystone default_project_id is pointed here when it is still
	// unset, so a dashboard login lands in the project instead of nowhere.
	PrimaryProject bool
}

// SyncProjectMembers reconciles a project's direct user role assignments to match
// the desired list. The algorithm:
//
//   - Users in desired but absent from the project are found (or created) and assigned.
//   - Users in desired with a wrong role have their old role removed and the new one added.
//   - Users currently in the project whose email contains "@" (real users) but are absent
//     from desired have all their direct role assignments removed.
//   - Service accounts (names without "@") are never touched.
//
// All errors are non-fatal: a failure for one user is logged and the sync continues.
func (c *OpenStackClient) SyncProjectMembers(projectID string, desired []DesiredMember) ([]PreseedConflict, error) {
	return c.syncProjectMembers(projectID, desired, true)
}

// syncProjectMembers returns the accounts that could not be resolved without
// guessing (see PreseedConflict) alongside the usual error: the run continues
// for everyone else, but those users need a human and must not vanish into the
// log.
func (c *OpenStackClient) syncProjectMembers(projectID string, desired []DesiredMember, removeUnwanted bool) ([]PreseedConflict, error) {
	var conflicts []PreseedConflict
	// Build desired map: lowercased email → entry with original casing + role.
	type desiredEntry struct {
		email    string
		roleName string
		primary  bool
	}
	desiredMap := make(map[string]desiredEntry, len(desired))
	for _, m := range desired {
		if looksLikeEmail(m.Email) {
			desiredMap[strings.ToLower(m.Email)] = desiredEntry{
				email: m.Email, roleName: m.RoleName, primary: m.PrimaryProject,
			}
		}
	}

	// Fetch current direct-user assignments.
	currentAssignments, err := c.ListProjectMembers(projectID)
	if err != nil {
		return nil, fmt.Errorf("list current members: %w", err)
	}

	// Build current map: lowercased email → slice of {userID, roleID, roleName}.
	// A user may have multiple roles; we track all of them so we can clean up extras.
	type roleEntry struct {
		userID   string
		roleID   string
		roleName string
	}
	currentByEmail := make(map[string][]roleEntry, len(currentAssignments))
	for _, pr := range currentAssignments {
		if pr.UserID == "" {
			continue // group assignment — not managed here
		}
		user, err := c.GetUserByID(pr.UserID)
		if err != nil || user == nil {
			c.log.Warnw("Could not resolve user for member sync, skipping",
				"user_id", pr.UserID, "project_id", projectID)
			continue
		}
		email := resolveUserEmail(user)
		if !looksLikeEmail(email) {
			continue // service account
		}
		k := strings.ToLower(email)
		currentByEmail[k] = append(currentByEmail[k], roleEntry{
			userID: pr.UserID, roleID: pr.RoleID, roleName: pr.RoleName,
		})
	}

	// Role name → ID cache to reduce API calls.
	roleIDCache := make(map[string]string)
	findRoleID := func(name string) (string, error) {
		if id, ok := roleIDCache[name]; ok {
			return id, nil
		}
		role, err := c.FindRoleByName(name)
		if err != nil {
			return "", fmt.Errorf("find role %q: %w", name, err)
		}
		roleIDCache[name] = role.ID
		return role.ID, nil
	}

	// ── Add / update ─────────────────────────────────────────────────────────
	for emailLower, want := range desiredMap {
		curEntries := currentByEmail[emailLower]

		// Check if correct role is already assigned.
		alreadyCorrect := false
		for _, e := range curEntries {
			if strings.EqualFold(e.roleName, want.roleName) {
				alreadyCorrect = true
			}
		}

		// Remove any incorrect role assignments for this user.
		for _, e := range curEntries {
			if strings.EqualFold(e.roleName, want.roleName) {
				continue // keep correct assignment
			}
			if err := c.RemoveProjectMember(projectID, e.userID, e.roleID); err != nil {
				c.log.Warnw("Could not remove stale role",
					"email", want.email, "role", e.roleName, "project_id", projectID, "error", err)
			}
		}

		if alreadyCorrect {
			// The role needs no change, but a member added before this ran (or
			// before the owner changed) can still be missing a default project.
			// The user id is already known from the existing assignment, so this
			// costs one GET and, at most once per user, one PATCH.
			if want.primary && len(curEntries) > 0 {
				c.ensureDefaultProject(curEntries[0].userID, projectID, want.email)
			}
			continue
		}

		user, err := c.FindOrCreateUser(want.email)
		if err != nil {
			var conflict *PreseedConflict
			if errors.As(err, &conflict) {
				conflicts = append(conflicts, *conflict)
				c.log.Warnw("Pre-seeding conflict — role NOT assigned",
					"email", want.email, "project_id", projectID, "reason", conflict.Reason)
			} else {
				c.log.Warnw("Could not find/create user, skipping",
					"email", want.email, "project_id", projectID, "error", err)
			}
			continue
		}
		roleID, err := findRoleID(want.roleName)
		if err != nil {
			c.log.Warnw("Role not found in OpenStack, skipping assignment",
				"email", want.email, "role", want.roleName, "error", err)
			continue
		}
		if err := c.AddProjectMember(projectID, user.ID, roleID); err != nil {
			c.log.Warnw("Could not assign role",
				"email", want.email, "role", want.roleName, "project_id", projectID, "error", err)
		} else {
			c.log.Infow("Assigned project role",
				"email", want.email, "role", want.roleName, "project_id", projectID)
		}
		if want.primary {
			c.ensureDefaultProject(user.ID, projectID, want.email)
		}
	}

	// ── Remove users no longer desired ───────────────────────────────────────
	if removeUnwanted {
		for emailLower, entries := range currentByEmail {
			if _, keep := desiredMap[emailLower]; keep {
				continue
			}
			for _, e := range entries {
				if err := c.RemoveProjectMember(projectID, e.userID, e.roleID); err != nil {
					c.log.Warnw("Could not remove member",
						"email", emailLower, "role", e.roleName, "project_id", projectID, "error", err)
				} else {
					c.log.Infow("Removed member from project",
						"email", emailLower, "role", e.roleName, "project_id", projectID)
				}
			}
		}
	}

	return conflicts, nil
}

// ListProjectMemberInfo returns all role assignments for a project with resolved user emails.
// Service accounts or users whose email cannot be determined are silently skipped.
func (c *OpenStackClient) ListProjectMemberInfo(projectID string) ([]ProjectMemberInfo, error) {
	members, err := c.ListProjectMembers(projectID)
	if err != nil {
		return nil, err
	}

	out := make([]ProjectMemberInfo, 0, len(members))
	for _, m := range members {
		user, err := c.GetUserByID(m.UserID)
		if err != nil || user == nil {
			c.log.Warnw("Could not resolve user for role assignment, skipping",
				"user_id", m.UserID, "project_id", projectID, "error", err)
			continue
		}
		email := resolveUserEmail(user)
		if email == "" {
			continue
		}
		out = append(out, ProjectMemberInfo{Email: email, RoleName: m.RoleName})
	}
	return out, nil
}

// GroupProjectRole represents a group's role assignment within a project.
type GroupProjectRole struct {
	GroupID  string
	RoleID   string
	RoleName string
}

// ListProjectGroupRoles returns all group (not user) role assignments for a project.
// User assignments are ignored; only entries where Group.ID is set are returned.
func (c *OpenStackClient) ListProjectGroupRoles(projectID string) ([]GroupProjectRole, error) {
	listOpts := roles.ListAssignmentsOpts{ScopeProjectID: projectID}
	iter := NewPagerIterator(
		func() pagination.Pager { return roles.ListAssignments(c.Identity, listOpts) },
		roles.ExtractRoleAssignments,
	)

	var result []GroupProjectRole
	for {
		assignment, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("failed to list group role assignments: %w", err)
		}
		if assignment == nil {
			break
		}
		if assignment.Group.ID == "" {
			continue // user assignment — not handled here
		}
		role, err := roles.Get(c.Identity, assignment.Role.ID).Extract()
		roleName := assignment.Role.ID
		if err == nil && role != nil {
			roleName = role.Name
		}
		result = append(result, GroupProjectRole{
			GroupID:  assignment.Group.ID,
			RoleID:   assignment.Role.ID,
			RoleName: roleName,
		})
	}
	return result, nil
}

// ListProjectMembers returns all role assignments in a project
func (c *OpenStackClient) ListProjectMembers(projectID string) ([]ProjectRole, error) {
	listOpts := roles.ListAssignmentsOpts{ScopeProjectID: projectID}
	iter := NewPagerIterator(
		func() pagination.Pager { return roles.ListAssignments(c.Identity, listOpts) },
		roles.ExtractRoleAssignments,
	)

	var members []ProjectRole
	for {
		assignment, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("failed to list role assignments: %w", err)
		}
		if assignment == nil {
			break
		}

		role, err := roles.Get(c.Identity, assignment.Role.ID).Extract()
		roleName := assignment.Role.ID
		if err == nil && role != nil {
			roleName = role.Name
		}

		members = append(members, ProjectRole{
			UserID:    assignment.User.ID,
			ProjectID: projectID,
			RoleID:    assignment.Role.ID,
			RoleName:  roleName,
		})
	}

	return members, nil
}

// AddProjectMember assigns a role to a user in a project
func (c *OpenStackClient) AddProjectMember(projectID, userID, roleID string) error {
	err := roles.Assign(c.Identity, roleID, roles.AssignOpts{UserID: userID, ProjectID: projectID}).ExtractErr()
	if err != nil {
		return fmt.Errorf("assign role: %w", err)
	}
	return nil
}

// RemoveProjectMember removes a role from a user in a project
func (c *OpenStackClient) RemoveProjectMember(projectID, userID, roleID string) error {
	err := roles.Unassign(c.Identity, roleID, roles.UnassignOpts{UserID: userID, ProjectID: projectID}).ExtractErr()
	if err != nil {
		return fmt.Errorf("unassign role: %w", err)
	}
	return nil
}

// FindRoleByName finds a role by its name
func (c *OpenStackClient) FindRoleByName(roleName string) (*roles.Role, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return roles.List(c.Identity, roles.ListOpts{Name: roleName}) },
		roles.ExtractRoles,
	)

	role, err := iter.Next()
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, fmt.Errorf("role %q not found", roleName)
	}

	return role, nil
}
