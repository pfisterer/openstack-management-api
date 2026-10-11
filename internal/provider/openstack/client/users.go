package osclient

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/pagination"
	"go.uber.org/zap"
)

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
	if c.federatedProvisioning {
		// A federated account lives in the IdP's domain, which is not the
		// one the service user manages: look there, not in the token's.
		existing, err := c.findUserInDomain(email, c.federatedDomainID)
		if err != nil {
			return nil, fmt.Errorf("look up user %q: %w", email, err)
		}
		return c.findOrCreateFederatedUser(email, existing)
	}

	existing, err := c.FindUserByName(email)
	if err != nil {
		return nil, fmt.Errorf("look up user %q: %w", email, err)
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
	// In the IdP's domain, where the login looks for it. Left out, Keystone
	// puts the account in the token's domain — for a service user scoped to
	// its own domain that is the wrong one, and the account arrives there
	// without the federation link, so no login ever uses it.
	created, err := users.Create(c.Identity, users.CreateOpts{
		Name:        name,
		DomainID:    c.federatedDomainID,
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
	email, _ := user.Extra["email"].(string)
	// A plain address wins, from the e-mail field or else the name. Some
	// accounts carry "Bennet Frey (s123@example.edu)" as their e-mail while
	// their name is the address; read as it is, the member sync took that
	// string for an address the tree could never name, and removed them.
	for _, candidate := range []string{email, user.Name} {
		if plainAddress(candidate) {
			return strings.TrimSpace(candidate)
		}
	}
	// Otherwise the address inside such a display form.
	for _, candidate := range []string{email, user.Name} {
		if m := enclosedAddress.FindStringSubmatch(candidate); m != nil {
			return m[1]
		}
	}
	if email != "" {
		return email
	}
	return user.Name
}

// enclosedAddress finds an address in "Name (addr)" or "Name <addr>".
var enclosedAddress = regexp.MustCompile(`[(<]\s*([^\s()<>@]+@[^\s()<>@]+)\s*[)>]`)

// plainAddress reports whether s is a bare e-mail address, nothing around it.
func plainAddress(s string) bool {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// findUserInDomain finds a user by name in the given domain; nil when there is
// none.
func (c *OpenStackClient) findUserInDomain(userName, domainID string) (*users.User, error) {
	iter := NewPagerIterator(
		func() pagination.Pager {
			return users.List(c.userLookupSvc(), users.ListOpts{Name: userName, DomainID: domainID})
		},
		users.ExtractUsers,
	)
	return iter.Next()
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

// OrphanedUserFlagDescription was set on managed users without project memberships
// while the reconciler had a no-delete mode. Users still carrying it are recognised
// as managed, so the orphan sweep removes them like any other.
const OrphanedUserFlagDescription = "PENDING DELETION (no project memberships) - Auto-created by openstack-management-api (do not edit or delete this comment)"

// ListUserProjectAssignments returns all project-scoped role assignments for a user,
// INCLUDING the ones they hold through a group.
// An empty slice (no error) means the user has no project memberships anywhere.
//
// `effective` is what makes group membership count. Without it Keystone reports
// only assignments written directly against the user id, and this function feeds
// CollectOrphanedManagedUsers — which deletes managed accounts that appear to
// hold no role. A user authorised solely through a Keystone group would look
// unused and be deleted, on every reconcile pass. That has not bitten yet only
// because no groups carry role assignments in these environments today;
// syncGroupMembers already puts users into groups, so it was waiting to.
func (c *OpenStackClient) ListUserProjectAssignments(userID string) ([]ProjectRole, error) {
	effective := true
	listOpts := roles.ListAssignmentsOpts{UserID: userID, Effective: &effective}
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
