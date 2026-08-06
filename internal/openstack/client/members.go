package osclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/pagination"
	"go.uber.org/zap"
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

// looksLikeEmail returns true when the string contains "@", distinguishing real user
// accounts from Keystone service accounts (nova, cinder, neutron, etc.).
func looksLikeEmail(s string) bool {
	return strings.Contains(s, "@")
}

// FindOrCreateUser finds a Keystone user by name/email, creating them if not found.
//
// The new user is created with no password — they authenticate via SSO (Keycloak OIDC or
// similar). How the account is created depends on the target Keystone's OIDC mapping, and
// getting this wrong silently orphans the role assignment:
//
//   - Ephemeral mapping (Keystone's default when the mapping sets no user "type"): a plain
//     local user is IGNORED on SSO login. Keystone creates a separate shadow user keyed by
//     (idp_id, protocol_id, unique_id), so any role assigned to the pre-created local user
//     never applies. To pre-seed roles here the account must itself be a federated user
//     carrying those attributes, so the login reuses it. Enable via SetFederationConfig
//     (OPENSTACK_FEDERATED_PROVISIONING=true).
//   - type:local mapping: the login binds to an existing local user by name, so a plain
//     local user is correct and federated provisioning is unnecessary.
//
// When federated provisioning is on, the unique_id is the URL-encoded value the IdP asserts
// for the user — the preferred_username claim under the default DHBW mapping, which for these
// accounts is the email (e.g. "s123%40example.edu"). If the IdP asserts something other than
// the email, this account simply won't be matched and the login falls back to auto-creating
// its own shadow user (no worse than not pre-creating at all).
func (c *OpenStackClient) FindOrCreateUser(email string) (*users.User, error) {
	existing, err := c.FindUserByName(email)
	if err != nil {
		return nil, fmt.Errorf("look up user %q: %w", email, err)
	}

	if c.federatedProvisioning {
		return c.findOrCreateFederatedUser(email, existing)
	}

	if existing != nil {
		return existing, nil
	}

	// Password is intentionally empty — SSO-only login.
	created, err := c.CreateUser(email, "", email, true)
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", email, err)
	}
	c.log.Infow("Created Keystone user", "email", email, "id", created.ID)
	return created, nil
}

// FederatedLink binds a pre-created user to an IdP identity so that an SSO login on an
// ephemeral-mapped Keystone reuses this account instead of spawning a separate shadow user.
// UniqueID is the URL-encoded value the IdP asserts (the mapping's remote attribute —
// preferred_username under the default DHBW mapping).
type FederatedLink struct {
	IdPID      string
	ProtocolID string
	UniqueID   string
}

// CreateFederatedUser creates a passwordless federated user carrying the given IdP link.
// gophercloud's users.CreateOpts has no typed federated field, but ToUserCreateMap merges
// the Extra map onto the user object, and "federated" is a first-class Keystone user
// attribute — so passing it via Extra produces the same request body as the REST API's
// native federated block. See FindOrCreateUser for when this path is used.
func (c *OpenStackClient) CreateFederatedUser(name, email string, link FederatedLink) (*users.User, error) {
	extra := map[string]any{
		"federated": []map[string]any{{
			"idp_id": link.IdPID,
			"protocols": []map[string]any{{
				"protocol_id": link.ProtocolID,
				"unique_id":   link.UniqueID,
			}},
		}},
	}
	if email != "" {
		extra["email"] = email
	}
	created, err := users.Create(c.Identity, users.CreateOpts{
		Name:        name,
		Enabled:     gophercloud.Enabled,
		Description: ManagedUserDescription,
		Extra:       extra,
	}).Extract()
	if err != nil {
		return nil, err
	}
	c.log.Infow("Created federated Keystone user",
		"email", email, "id", created.ID, "idp", link.IdPID, "protocol", link.ProtocolID)
	return created, nil
}

// ensureDefaultProject points a user's Keystone default_project_id at projectID,
// but ONLY when it is still unset.
//
// Without it a dashboard login yields an unscoped session: the person has the
// role, yet lands with no project selected and has to find it in the switcher.
//
// The "only when unset" part is not politeness, it is correctness. This runs on
// every reconcile pass (every few minutes) for every project. Overwriting would
// mean that someone who owns two projects has their choice reset continuously,
// and whichever project happened to sync last would win. The first project a
// person owns therefore sets the default and nothing ever changes it again;
// picking a different one is theirs to do.
//
// Failures are logged, never fatal — a missing default project is a small
// inconvenience, and it must not abort a member sync that otherwise worked.
func (c *OpenStackClient) ensureDefaultProject(userID, projectID, email string) {
	user, err := c.GetUserByID(userID)
	if err != nil || user == nil {
		c.log.Warnw("Could not read user for default-project check",
			"email", email, "user_id", userID, "error", err)
		return
	}
	if user.DefaultProjectID != "" {
		return // already has one, theirs to change
	}
	if _, err := users.Update(c.Identity, userID, users.UpdateOpts{
		DefaultProjectID: projectID,
	}).Extract(); err != nil {
		c.log.Warnw("Could not set default project",
			"email", email, "user_id", userID, "project_id", projectID, "error", err)
		return
	}
	c.log.Infow("Set default project for user",
		"email", email, "user_id", userID, "project_id", projectID)
}

// EnsureProjectMembers adds or updates project role assignments to match desired, but
// never removes existing members. Use this when operating in no-delete mode.
func (c *OpenStackClient) EnsureProjectMembers(projectID string, desired []DesiredMember) ([]PreseedConflict, error) {
	return c.syncProjectMembers(projectID, desired, false)
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

// GetUserByID retrieves a single Keystone user by their ID.
func (c *OpenStackClient) GetUserByID(userID string) (*users.User, error) {
	user, err := users.Get(c.Identity, userID).Extract()
	if err != nil {
		return nil, fmt.Errorf("get user %s: %w", userID, err)
	}
	return user, nil
}

// resolveUserEmail extracts the best available email from a Keystone user.
// It checks the "email" key in Extra first (set by CreateUser), then falls back
// to the user's Name which is typically the email for OIDC-federated accounts.
func resolveUserEmail(user *users.User) string {
	if email, ok := user.Extra["email"].(string); ok && email != "" {
		return email
	}
	return user.Name
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

// FindUserByName finds a user by their name
func (c *OpenStackClient) FindUserByName(userName string) (*users.User, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return users.List(c.Identity, users.ListOpts{Name: userName}) },
		users.ExtractUsers,
	)

	user, err := iter.Next()
	if err != nil {
		return nil, err
	}
	// user == nil means not found; return nil, nil so callers can distinguish
	// "not found" from a real error and create the user if needed.
	return user, nil
}

// CreateUser creates a new user
func (c *OpenStackClient) CreateUser(name, password, email string, enabled bool) (*users.User, error) {
	extra := map[string]any{
		"tags": []string{"auto-created"},
	}
	if email != "" {
		extra["email"] = email
	}
	createOpts := users.CreateOpts{
		Name:             name,
		Password:         password,
		DefaultProjectID: "",
		Enabled:          gophercloud.Enabled,
		Description:      ManagedUserDescription,
		Extra:            extra,
	}
	if !enabled {
		createOpts.Enabled = gophercloud.Disabled
	}

	user, err := users.Create(c.Identity, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	c.logger.Info("User created", zap.String("name", name), zap.String("id", user.ID))
	return user, nil
}

// ListUsers returns all users
func (c *OpenStackClient) ListUsers() ([]users.User, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return users.List(c.Identity, nil) },
		users.ExtractUsers,
	)
	return CollectIterator(iter)
}

// UpdateUserDescription updates the description field of a Keystone user.
func (c *OpenStackClient) UpdateUserDescription(userID, description string) error {
	_, err := users.Update(c.Identity, userID, users.UpdateOpts{
		Description: &description,
	}).Extract()
	if err != nil {
		return fmt.Errorf("update user %s: %w", userID, err)
	}
	return nil
}

// DeleteUser permanently removes a Keystone user by their ID.
func (c *OpenStackClient) DeleteUser(userID string) error {
	if err := users.Delete(c.Identity, userID).ExtractErr(); err != nil {
		return fmt.Errorf("delete user %s: %w", userID, err)
	}
	c.log.Infow("Deleted Keystone user", "user_id", userID)
	return nil
}

// ManagedUserDescription is the description set on every user auto-created by this system.
// It is the only reliable marker available — Keystone users do not support tags.
const ManagedUserDescription = "Auto-created by openstack-management-api (do not edit or delete this comment)"

// OrphanedUserFlagDescription is set on managed users that have no project memberships
// when the reconciler runs in NoDelete mode. It signals external operators that the
// account is safe to remove once NoDelete mode is lifted.
const OrphanedUserFlagDescription = "PENDING DELETION (no project memberships) - Auto-created by openstack-management-api (do not edit or delete this comment)"

// ListUserProjectAssignments returns all project-scoped role assignments for a user.
// An empty slice (no error) means the user has no project memberships anywhere.
func (c *OpenStackClient) ListUserProjectAssignments(userID string) ([]ProjectRole, error) {
	listOpts := roles.ListAssignmentsOpts{UserID: userID}
	iter := NewPagerIterator(
		func() pagination.Pager { return roles.ListAssignments(c.Identity, listOpts) },
		roles.ExtractRoleAssignments,
	)

	var out []ProjectRole
	for {
		assignment, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("list role assignments for user %s: %w", userID, err)
		}
		if assignment == nil {
			break
		}
		if assignment.Scope.Project.ID == "" {
			continue // domain-scoped assignment — not a project membership
		}
		out = append(out, ProjectRole{
			UserID:    userID,
			ProjectID: assignment.Scope.Project.ID,
			RoleID:    assignment.Role.ID,
		})
	}
	return out, nil
}

// CollectOrphanedManagedUsers returns all users that were auto-created by this system
// and currently hold no project role assignments anywhere in OpenStack.
// These are safe to delete: they have no resources and were created solely for SSO
// pre-seeding when a project was approved.
func (c *OpenStackClient) CollectOrphanedManagedUsers() ([]users.User, error) {
	all, err := c.ListUsers()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	var orphans []users.User
	for _, u := range all {
		if u.Description != ManagedUserDescription && u.Description != OrphanedUserFlagDescription {
			continue // not created by us
		}
		assignments, err := c.ListUserProjectAssignments(u.ID)
		if err != nil {
			c.log.Warnw("Could not check assignments for managed user, skipping",
				"user_id", u.ID, "name", u.Name, "error", err)
			continue
		}
		if len(assignments) == 0 {
			orphans = append(orphans, u)
		}
	}
	return orphans, nil
}
