package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/backups"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/pagination"
)

// What emptying a project before its deletion needs: list what is in it, delete
// it piece by piece. Every listing carries the owning project of each item, so
// the caller can check it — a filter a service ignores must never turn into
// deleting somebody else's resources.

// Resource is one thing in a project, as far as deleting it needs to know.
type Resource struct {
	ID        string
	Name      string
	ProjectID string
	Status    string
	// Ports only: what the port belongs to, e.g. "network:router_interface"
	// and the router's ID.
	DeviceOwner string
	DeviceID    string
}

// slicePage is what every gophercloud page offers to decode its list into a
// struct of our choosing — needed where the library's own type leaves out the
// owning project.
type slicePage interface {
	ExtractIntoSlicePtr(v interface{}, label string) error
}

func extractInto(page pagination.Page, label string, v interface{}) error {
	p, ok := page.(slicePage)
	if !ok {
		return fmt.Errorf("unexpected page type %T", page)
	}
	return p.ExtractIntoSlicePtr(v, label)
}

func listResources(pager pagination.Pager, what, projectID string, each func(pagination.Page) ([]Resource, error)) ([]Resource, error) {
	var out []Resource
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		items, err := each(page)
		if err != nil {
			return false, err
		}
		out = append(out, items...)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s of %s: %w", what, projectID, err)
	}
	return out, nil
}

// ── Compute ───────────────────────────────────────────────────────────────────

// DeleteServer deletes a server, shelved or not. Nova removes it in the
// background; the next listing shows whether it is gone.
func (c *OpenStackClient) DeleteServer(id string) error {
	if err := servers.Delete(c.computeSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete server %s: %w", id, err)
	}
	return nil
}

// ── Block storage ─────────────────────────────────────────────────────────────

// ListProjectVolumes lists the volumes of a project.
func (c *OpenStackClient) ListProjectVolumes(projectID string) ([]Resource, error) {
	return listResources(volumes.List(c.blockSvc(), volumes.ListOpts{AllTenants: true, TenantID: projectID}), "volumes", projectID,
		func(page pagination.Page) ([]Resource, error) {
			var list []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				ProjectID string `json:"os-vol-tenant-attr:tenant_id"`
			}
			if err := extractInto(page, "volumes", &list); err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, v := range list {
				out = append(out, Resource{ID: v.ID, Name: v.Name, Status: v.Status, ProjectID: v.ProjectID})
			}
			return out, nil
		})
}

// DeleteVolume deletes a volume together with its snapshots.
func (c *OpenStackClient) DeleteVolume(id string) error {
	if err := volumes.Delete(c.blockSvc(), id, volumes.DeleteOpts{Cascade: true}).ExtractErr(); err != nil {
		return fmt.Errorf("delete volume %s: %w", id, err)
	}
	return nil
}

// ListProjectSnapshots lists the volume snapshots of a project. From the
// detailed listing: the plain one, which is all gophercloud offers, leaves out
// the owning project, so every snapshot looked like somebody else's and was
// skipped — one stood up only because deleting its volume took it along.
func (c *OpenStackClient) ListProjectSnapshots(projectID string) ([]Resource, error) {
	query, err := snapshots.ListOpts{AllTenants: true, TenantID: projectID}.ToSnapshotListQuery()
	if err != nil {
		return nil, err
	}
	url := c.blockSvc().ServiceURL("snapshots", "detail") + query
	pager := pagination.NewPager(c.blockSvc(), url, func(r pagination.PageResult) pagination.Page {
		return snapshots.SnapshotPage{LinkedPageBase: pagination.LinkedPageBase{PageResult: r}}
	})
	return listResources(pager, "snapshots", projectID,
		func(page pagination.Page) ([]Resource, error) {
			var list []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				ProjectID string `json:"os-extended-snapshot-attributes:project_id"`
			}
			if err := extractInto(page, "snapshots", &list); err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, s := range list {
				out = append(out, Resource{ID: s.ID, Name: s.Name, Status: s.Status, ProjectID: s.ProjectID})
			}
			return out, nil
		})
}

// DeleteSnapshot deletes a volume snapshot.
func (c *OpenStackClient) DeleteSnapshot(id string) error {
	if err := snapshots.Delete(c.blockSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", id, err)
	}
	return nil
}

// ListProjectBackups lists the volume backups of a project. The detailed
// listing has no project filter, so it reads all of them and keeps the
// project's — backups are few.
func (c *OpenStackClient) ListProjectBackups(projectID string) ([]Resource, error) {
	return listResources(backups.ListDetail(c.blockSvc(), backups.ListDetailOpts{AllTenants: true}), "backups", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := backups.ExtractBackups(page)
			if err != nil {
				return nil, err
			}
			var out []Resource
			for _, b := range list {
				if b.ProjectID == projectID {
					out = append(out, Resource{ID: b.ID, Name: b.Name, Status: b.Status, ProjectID: b.ProjectID})
				}
			}
			return out, nil
		})
}

// DeleteBackup deletes a volume backup.
func (c *OpenStackClient) DeleteBackup(id string) error {
	if err := backups.Delete(c.blockSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete backup %s: %w", id, err)
	}
	return nil
}

// ── Images ────────────────────────────────────────────────────────────────────

// ListProjectImages lists the images a project owns — uploads, server
// snapshots, and the images shelving leaves behind.
func (c *OpenStackClient) ListProjectImages(projectID string) ([]Resource, error) {
	return listResources(images.List(c.imageSvc(), images.ListOpts{Owner: projectID}), "images", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := images.ExtractImages(page)
			if err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, i := range list {
				out = append(out, Resource{ID: i.ID, Name: i.Name, Status: string(i.Status), ProjectID: i.Owner})
			}
			return out, nil
		})
}

// DeleteImage deletes an image.
func (c *OpenStackClient) DeleteImage(id string) error {
	if err := images.Delete(c.imageSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete image %s: %w", id, err)
	}
	return nil
}

// ── Network ───────────────────────────────────────────────────────────────────

// ListProjectRouters lists the routers of a project.
func (c *OpenStackClient) ListProjectRouters(projectID string) ([]Resource, error) {
	return listResources(routers.List(c.networkSvc(), routers.ListOpts{ProjectID: projectID}), "routers", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := routers.ExtractRouters(page)
			if err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, r := range list {
				out = append(out, Resource{ID: r.ID, Name: r.Name, Status: r.Status, ProjectID: firstNonEmpty(r.ProjectID, r.TenantID)})
			}
			return out, nil
		})
}

// RemoveRouterInterface detaches a router from a subnet, by the port joining them.
func (c *OpenStackClient) RemoveRouterInterface(routerID, portID string) error {
	if _, err := routers.RemoveInterface(c.networkSvc(), routerID, routers.RemoveInterfaceOpts{PortID: portID}).Extract(); err != nil {
		return fmt.Errorf("remove interface %s from router %s: %w", portID, routerID, err)
	}
	return nil
}

// DeleteRouter deletes a router; its external gateway goes with it.
func (c *OpenStackClient) DeleteRouter(id string) error {
	if err := routers.Delete(c.networkSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete router %s: %w", id, err)
	}
	return nil
}

// ListProjectPorts lists the ports of a project.
func (c *OpenStackClient) ListProjectPorts(projectID string) ([]Resource, error) {
	return listResources(ports.List(c.networkSvc(), ports.ListOpts{ProjectID: projectID}), "ports", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := ports.ExtractPorts(page)
			if err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, p := range list {
				out = append(out, Resource{ID: p.ID, Name: p.Name, Status: p.Status, ProjectID: firstNonEmpty(p.ProjectID, p.TenantID),
					DeviceOwner: p.DeviceOwner, DeviceID: p.DeviceID})
			}
			return out, nil
		})
}

// DeletePort deletes a port.
func (c *OpenStackClient) DeletePort(id string) error {
	if err := ports.Delete(c.networkSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete port %s: %w", id, err)
	}
	return nil
}

// ListProjectNetworks lists the networks of a project.
func (c *OpenStackClient) ListProjectNetworks(projectID string) ([]Resource, error) {
	return listResources(networks.List(c.networkSvc(), networks.ListOpts{ProjectID: projectID}), "networks", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := networks.ExtractNetworks(page)
			if err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, n := range list {
				out = append(out, Resource{ID: n.ID, Name: n.Name, Status: n.Status, ProjectID: firstNonEmpty(n.ProjectID, n.TenantID)})
			}
			return out, nil
		})
}

// DeleteNetwork deletes a network with its subnets.
func (c *OpenStackClient) DeleteNetwork(id string) error {
	if err := networks.Delete(c.networkSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete network %s: %w", id, err)
	}
	return nil
}

// ListProjectSecurityGroups lists the security groups of a project.
func (c *OpenStackClient) ListProjectSecurityGroups(projectID string) ([]Resource, error) {
	return listResources(groups.List(c.networkSvc(), groups.ListOpts{ProjectID: projectID}), "security groups", projectID,
		func(page pagination.Page) ([]Resource, error) {
			list, err := groups.ExtractGroups(page)
			if err != nil {
				return nil, err
			}
			out := make([]Resource, 0, len(list))
			for _, g := range list {
				out = append(out, Resource{ID: g.ID, Name: g.Name, ProjectID: firstNonEmpty(g.ProjectID, g.TenantID)})
			}
			return out, nil
		})
}

// DeleteSecurityGroup deletes a security group.
func (c *OpenStackClient) DeleteSecurityGroup(id string) error {
	if err := groups.Delete(c.networkSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("delete security group %s: %w", id, err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
