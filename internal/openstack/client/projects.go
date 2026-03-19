package osclient

import (
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/pagination"
	"go.uber.org/zap"
)

// BaseProjectOpts contains common project options
type BaseProjectOpts struct {
	Name        string
	Description *string
	DomainID    string
	Enabled     *bool
	ParentID    string
}

// ProjectCreateOpts contains options for creating a project
type ProjectCreateOpts struct {
	BaseProjectOpts
	Tags []string
}

// ProjectUpdateOpts contains options for updating a project
type ProjectUpdateOpts struct {
	BaseProjectOpts
	IsDomain *bool
}

// ListProjects returns an iterator for all projects
func (c *OpenStackClient) ListProjects() Iterator[projects.Project] {
	return NewPagerIterator(
		func() pagination.Pager {
			return projects.List(c.Identity, nil)
		},
		projects.ExtractProjects,
	)
}

// ListProjectsFiltered returns an iterator for projects matching the given options
func (c *OpenStackClient) ListProjectsFiltered(opts projects.ListOpts) Iterator[projects.Project] {
	return NewPagerIterator(
		func() pagination.Pager {
			return projects.List(c.Identity, opts)
		},
		projects.ExtractProjects,
	)
}

// GetProject retrieves a specific project by ID
func (c *OpenStackClient) GetProject(projectID string) (*projects.Project, error) {
	return projects.Get(c.Identity, projectID).Extract()
}

// FindProjectByName finds a project by its name
func (c *OpenStackClient) FindProjectByName(projectName string) (*projects.Project, error) {
	iter := NewPagerIterator(
		func() pagination.Pager { return projects.List(c.Identity, projects.ListOpts{Name: projectName}) },
		projects.ExtractProjects,
	)

	project, err := iter.Next()
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, nil // Not found
	}

	return project, nil
}

// CreateProject creates a new project
func (c *OpenStackClient) CreateProject(opts ProjectCreateOpts) (*projects.Project, error) {
	createOpts := projects.CreateOpts{
		Name:        opts.Name,
		Description: derefString(opts.Description),
		DomainID:    opts.DomainID,
		Enabled:     opts.Enabled,
		ParentID:    opts.ParentID,
		Tags:        opts.Tags,
	}

	project, err := projects.Create(c.Identity, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	c.logger.Info("Project created", zap.String("name", project.Name), zap.String("id", project.ID))
	return project, nil
}

// UpdateProject updates an existing project
func (c *OpenStackClient) UpdateProject(projectID string, opts ProjectUpdateOpts) (*projects.Project, error) {
	updateOpts := projects.UpdateOpts{
		Name:        opts.Name,
		Description: opts.Description,
		Enabled:     opts.Enabled,
		DomainID:    opts.DomainID,
		ParentID:    opts.ParentID,
		IsDomain:    opts.IsDomain,
	}

	project, err := projects.Update(c.Identity, projectID, updateOpts).Extract()
	if err != nil {
		return nil, err
	}
	c.logger.Info("Project updated", zap.String("id", projectID))
	return project, nil
}

// DeleteProject deletes a project (simple deletion)
func (c *OpenStackClient) DeleteProject(projectID string) error {
	err := projects.Delete(c.Identity, projectID).ExtractErr()
	if err != nil {
		return err
	}
	c.logger.Info("Project deleted", zap.String("id", projectID))
	return nil
}

// derefString dereferences a pointer to string, returning empty string if nil
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
