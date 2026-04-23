package osclient

import (
	"strings"

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
	// Tags replaces the project's full tag list when non-nil.
	Tags *[]string
}

// ExtractResourceIDFromTags scans a project tag list for the resource-id tag and returns
// the embedded project ID. Returns empty string when no matching tag is found.
func (c *OpenStackClient) ExtractResourceIDFromTags(tags []string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, c.resourceIDTagPrefix) {
			return strings.TrimPrefix(tag, c.resourceIDTagPrefix)
		}
	}
	return ""
}

// IsManagedProject reports whether the project was created by this system.
func (c *OpenStackClient) IsManagedProject(tags []string) bool {
	for _, tag := range tags {
		if tag == c.managedProjectTag {
			return true
		}
	}
	return false
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
		Tags:        opts.Tags,
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

// ProjectInfo carries only the fields the reconciler needs from an OpenStack project.
// It keeps gophercloud types contained inside the osclient package.
type ProjectInfo struct {
	ID   string
	Name string
	Tags []string
}

// ListManagedProjects returns an iterator of all projects tagged with the configured managed-project tag.
// These are projects created and controlled by this system.
func (c *OpenStackClient) ListManagedProjects() Iterator[projects.Project] {
	return NewPagerIterator(
		func() pagination.Pager {
			return projects.List(c.Identity, projects.ListOpts{Tags: c.managedProjectTag})
		},
		projects.ExtractProjects,
	)
}

// ListProjectsByParent returns an iterator of all projects whose parent is parentID.
// Use this to scope reconciliation to a specific OpenStack project hierarchy.
func (c *OpenStackClient) ListProjectsByParent(parentID string) Iterator[projects.Project] {
	return NewPagerIterator(
		func() pagination.Pager {
			return projects.List(c.Identity, projects.ListOpts{ParentID: parentID})
		},
		projects.ExtractProjects,
	)
}

// CollectManagedProjects drains all managed projects into a flat slice of ProjectInfo.
func (c *OpenStackClient) CollectManagedProjects() ([]ProjectInfo, error) {
	return drainToProjectInfo(c.ListManagedProjects())
}

// CollectProjectsByParent drains all projects under parentID into a flat slice.
func (c *OpenStackClient) CollectProjectsByParent(parentID string) ([]ProjectInfo, error) {
	return drainToProjectInfo(c.ListProjectsByParent(parentID))
}

func drainToProjectInfo(iter Iterator[projects.Project]) ([]ProjectInfo, error) {
	raw, err := CollectIterator(iter)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectInfo, len(raw))
	for i, p := range raw {
		out[i] = ProjectInfo{ID: p.ID, Name: p.Name, Tags: p.Tags}
	}
	return out, nil
}

// CreateManagedProject creates a new project tagged with the managed marker and the
// project ID so the reconciler can identify it on subsequent runs.
func (c *OpenStackClient) CreateManagedProject(name, description, parentID, projectID string) (*projects.Project, error) {
	enabled := true
	tags := []string{c.managedProjectTag, c.resourceIDTagPrefix + projectID}
	opts := ProjectCreateOpts{
		BaseProjectOpts: BaseProjectOpts{
			Name:        name,
			Description: &description,
			Enabled:     &enabled,
			ParentID:    parentID,
		},
		Tags: tags,
	}
	return c.CreateProject(opts)
}

// TagProjectForPromotion adds the managed marker and a fresh resource-id tag to an
// existing OpenStack project so the reconciler picks it up as a managed project.
// Any pre-existing resource-id tag is replaced. Safe to call multiple times (idempotent
// apart from the new resource ID value). Existing non-resource-id tags are preserved.
func (c *OpenStackClient) TagProjectForPromotion(osProjectID, resourceID string, existingTags []string) error {
	newTags := make([]string, 0, len(existingTags)+2)
	hasManagedTag := false
	for _, tag := range existingTags {
		if tag == c.managedProjectTag {
			hasManagedTag = true
			newTags = append(newTags, tag)
			continue
		}
		if strings.HasPrefix(tag, c.resourceIDTagPrefix) {
			continue // replace stale resource-id tag
		}
		newTags = append(newTags, tag)
	}
	if !hasManagedTag {
		newTags = append(newTags, c.managedProjectTag)
	}
	newTags = append(newTags, c.resourceIDTagPrefix+resourceID)

	_, err := c.UpdateProject(osProjectID, ProjectUpdateOpts{Tags: &newTags})
	return err
}

// derefString dereferences a pointer to string, returning empty string if nil
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
