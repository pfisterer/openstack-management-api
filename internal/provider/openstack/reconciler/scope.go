package reconciler

import (
	"fmt"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// loadScopedOSProjects fetches the OS projects to reconcile against.
// When a scope parent is configured it lists all children of that parent so externally
// created projects can be imported. Otherwise only managed-tagged projects.
func (r *Reconciler) loadScopedOSProjects(scopeParentID string) ([]osclient.ProjectInfo, error) {
	if scopeParentID != "" {
		return r.osClient.CollectProjectsByParent(scopeParentID)
	}
	return r.osClient.CollectManagedProjects()
}

// scopeParentClient is the narrow OpenStack surface needed to resolve the scope parent.
// *osclient.OpenStackClient satisfies it; tests substitute a fake.
type scopeParentClient interface {
	FindProjectByName(name string) (*projects.Project, error)
	CreateProject(opts osclient.ProjectCreateOpts) (*projects.Project, error)
}

// ScopeParentDescription is written to the scope parent project when the reconciler
// creates it from ScopeParentName.
const ScopeParentDescription = "Parent project for all projects managed by the OpenStack management API. Do not modify manually."

// resolveScopeParent returns the OS project ID to scope reconciliation to.
// Precedence: explicit ScopeParentID > ScopeParentName (looked up by name, created on
// demand) > "" (unscoped, tag-based discovery only). In dry-run mode a missing named
// parent is NOT created and the run proceeds unscoped.
func resolveScopeParent(c scopeParentClient, cfg Config, log *zap.SugaredLogger) (string, error) {
	if cfg.ScopeParentID != "" {
		return cfg.ScopeParentID, nil
	}
	if cfg.ScopeParentName == "" {
		return "", nil
	}
	project, err := c.FindProjectByName(cfg.ScopeParentName)
	if err != nil {
		return "", fmt.Errorf("look up scope parent %q: %w", cfg.ScopeParentName, err)
	}
	if project != nil {
		return project.ID, nil
	}
	if cfg.DryRun {
		log.Infow("Would create scope parent project; proceeding unscoped",
			"name", cfg.ScopeParentName, "dry_run", true)
		return "", nil
	}
	// Deliberately no managed/resource-id tags: the scope parent must never look like
	// a managed leaf, or the import and deletion phases could touch it.
	description := ScopeParentDescription
	enabled := true
	created, err := c.CreateProject(osclient.ProjectCreateOpts{
		BaseProjectOpts: osclient.BaseProjectOpts{
			Name:        cfg.ScopeParentName,
			Description: &description,
			Enabled:     &enabled,
		},
	})
	if err != nil {
		// A concurrent creator may have won the race (409): the name is unique per
		// domain, so a successful re-lookup is authoritative.
		if again, ferr := c.FindProjectByName(cfg.ScopeParentName); ferr == nil && again != nil {
			return again.ID, nil
		}
		return "", fmt.Errorf("create scope parent %q: %w", cfg.ScopeParentName, err)
	}
	log.Infow("Created scope parent project",
		"name", cfg.ScopeParentName, "os_project_id", created.ID)
	return created.ID, nil
}

// ensureScopeParent resolves the effective scope parent and caches a successful,
// non-empty result for subsequent runs (the empty dry-run outcome is re-resolved
// every run so the project is picked up once it exists).
func (r *Reconciler) ensureScopeParent() (string, error) {
	if r.scopeParentID != "" {
		return r.scopeParentID, nil
	}
	id, err := resolveScopeParent(r.osClient, r.cfg, r.log)
	if err != nil {
		return "", err
	}
	r.scopeParentID = id
	r.scopeParentSeen.Store(&id)
	return id, nil
}
