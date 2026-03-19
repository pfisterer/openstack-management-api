package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/groups"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/pagination"
	"go.uber.org/zap"
)

// CreateGroup creates a new group
func (c *OpenStackClient) CreateGroup(name, description string) (*groups.Group, error) {
	createOpts := groups.CreateOpts{
		Name:        name,
		Description: description,
	}

	group, err := groups.Create(c.Identity, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	c.logger.Info("Group created", zap.String("name", name), zap.String("id", group.ID))
	return group, nil
}

// FindGroupByName finds a group by its name
func (c *OpenStackClient) FindGroupByName(groupName string) (*groups.Group, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return groups.List(c.Identity, groups.ListOpts{Name: groupName}) },
		groups.ExtractGroups,
	)

	group, err := iter.Next()
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, nil // Not found
	}

	return group, nil
}

// ListGroups returns all groups
func (c *OpenStackClient) ListGroups() ([]groups.Group, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return groups.List(c.Identity, nil) },
		groups.ExtractGroups,
	)
	return CollectIterator(iter)
}

// AddUserToGroup adds a user to a group
func (c *OpenStackClient) AddUserToGroup(groupID, userID string) error {
	err := users.AddToGroup(c.Identity, groupID, userID).ExtractErr()
	if err != nil {
		return fmt.Errorf("add user to group: %w", err)
	}
	return nil
}

// RemoveUserFromGroup removes a user from a group
func (c *OpenStackClient) RemoveUserFromGroup(groupID, userID string) error {
	err := users.RemoveFromGroup(c.Identity, groupID, userID).ExtractErr()
	if err != nil {
		return fmt.Errorf("remove user from group: %w", err)
	}
	return nil
}

// ListGroupUsers returns all users in a group
func (c *OpenStackClient) ListGroupUsers(groupID string) ([]string, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return users.ListInGroup(c.Identity, groupID, nil) },
		users.ExtractUsers,
	)

	userIDs := make([]string, 0)
	for {
		user, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if user == nil {
			return userIDs, nil
		}
		userIDs = append(userIDs, user.ID)
	}
}

// AssignGroupToProject assigns a role to a group in a project
func (c *OpenStackClient) AssignGroupToProject(projectID, groupID, roleID string) error {
	err := roles.Assign(c.Identity, roleID, roles.AssignOpts{GroupID: groupID, ProjectID: projectID}).ExtractErr()
	if err != nil {
		return fmt.Errorf("assign group role: %w", err)
	}
	return nil
}

// UnassignGroupFromProject removes a role from a group in a project
func (c *OpenStackClient) UnassignGroupFromProject(projectID, groupID, roleID string) error {
	err := roles.Unassign(c.Identity, roleID, roles.UnassignOpts{GroupID: groupID, ProjectID: projectID}).ExtractErr()
	if err != nil {
		return fmt.Errorf("unassign group role: %w", err)
	}
	return nil
}

// DeleteGroup deletes a group
func (c *OpenStackClient) DeleteGroup(groupID string) error {
	err := groups.Delete(c.Identity, groupID).ExtractErr()
	if err != nil {
		return err
	}
	c.logger.Info("Group deleted", zap.String("id", groupID))
	return nil
}
