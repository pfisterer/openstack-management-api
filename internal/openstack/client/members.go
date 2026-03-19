package osclient

import (
	"fmt"

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
	if user == nil {
		return nil, fmt.Errorf("user %q not found", userName)
	}

	return user, nil
}

// CreateUser creates a new user
func (c *OpenStackClient) CreateUser(name, password, email string, enabled bool) (*users.User, error) {
	createOpts := users.CreateOpts{
		Name:             name,
		Password:         password,
		DefaultProjectID: "",
		Enabled:          gophercloud.Enabled,
	}
	if !enabled {
		createOpts.Enabled = gophercloud.Disabled
	}
	if email != "" {
		createOpts.Extra = map[string]interface{}{"email": email}
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
