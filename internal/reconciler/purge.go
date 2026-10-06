package reconciler

import (
	"context"
	"slices"
	"strings"
	"time"

	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// purgeClient is the part of OpenStack emptying a project needs; tests
// substitute a fake.
type purgeClient interface {
	ListProjectServers(projectID string) ([]osclient.ServerInfo, error)
	DeleteServer(id string) error
	ListProjectFloatingIPs(projectID string) ([]osclient.FloatingIPInfo, error)
	ReleaseFloatingIP(id string) error
	ListProjectBackups(projectID string) ([]osclient.Resource, error)
	DeleteBackup(id string) error
	ListProjectSnapshots(projectID string) ([]osclient.Resource, error)
	DeleteSnapshot(id string) error
	ListProjectVolumes(projectID string) ([]osclient.Resource, error)
	DeleteVolume(id string) error
	ListProjectImages(projectID string) ([]osclient.Resource, error)
	DeleteImage(id string) error
	ListProjectRouters(projectID string) ([]osclient.Resource, error)
	RemoveRouterInterface(routerID, portID string) error
	DeleteRouter(id string) error
	ListProjectPorts(projectID string) ([]osclient.Resource, error)
	DeletePort(id string) error
	ListProjectNetworks(projectID string) ([]osclient.Resource, error)
	DeleteNetwork(id string) error
	ListProjectSecurityGroups(projectID string) ([]osclient.Resource, error)
	DeleteSecurityGroup(id string) error
	DeleteProject(projectID string) error
}

// purgeResult counts what one pass of emptying a project did.
type purgeResult struct {
	// deleted is true once the Keystone project itself is gone.
	deleted bool
	// stage names what the project is still waiting for, for the log.
	stage   string
	removed int
}

// deletionDue decides whether a retired leaf's project is deleted now.
func deletionDue(mode string, leaf tree.Node, tags []string, pendingPrefix string, now time.Time) bool {
	requested := slices.Contains(leaf.Flags, tree.FlagDeleteRequested)
	switch mode {
	case ReleasedDeleteOnRequest:
		return requested
	case ReleasedDeleteAfterGrace:
		if requested {
			return true
		}
		for _, t := range tags {
			if day, ok := strings.CutPrefix(t, pendingPrefix); ok {
				due, err := time.Parse(time.DateOnly, day)
				return err == nil && !now.UTC().Before(due)
			}
		}
		return false
	case ReleasedDeleteImmediately:
		return true
	}
	return false
}

// purgeRetiredProject empties the OpenStack project of a released or archived
// leaf and deletes it, then removes the leaf. Spread over passes: deleting is
// asynchronous in most services, and a stage only starts once the one before
// it is empty — volumes cannot go while servers still hold them, networks not
// while ports are on them. A pass does what it can and leaves the rest to the
// next one, so a project takes a few passes, and one that cannot be emptied
// (a protected image, a port somebody else put on its network) stays, named
// in the log on every pass, instead of being deleted with things left in it.
func (r *Reconciler) purgeRetiredProject(ctx context.Context, osProject osclient.ProjectInfo, leaf tree.Node, scopeParentID string, res *reconcileResult) {
	log := r.log.With("node_id", leaf.ID, "os_project_id", osProject.ID)
	out := purgeProject(r.osClient, osProject.ID, scopeParentID, r.cfg.DryRun, log)
	res.resourcesPurged += out.removed
	if !out.deleted {
		if out.stage != "refused" && !r.cfg.DryRun {
			res.purgesPending++
		}
		return
	}
	res.projectsDeleted++
	// The project is gone; the leaf goes with it. Should this fail, the next
	// pass finds the project missing and removes the leaf there.
	if _, err := r.store.DeleteNodeIf(ctx, leaf.ID, func(n tree.Node) bool { return tree.IsRetiredStatus(n.Status) }); err != nil {
		log.Warnw("Purge: project deleted, but the leaf could not be removed", "error", err)
		return
	}
	res.releasedLeavesRemoved++
}

func purgeProject(c purgeClient, projectID, scopeParentID string, dryRun bool, log *zap.SugaredLogger) purgeResult {
	if projectID == "" || projectID == scopeParentID {
		log.Errorw("Purge refused: not a project that may be deleted", "scope_parent_id", scopeParentID)
		return purgeResult{stage: "refused"}
	}
	p := purger{c: c, projectID: projectID, dryRun: dryRun, log: log}

	// One stage per pass at most; each returns false while it still has work.
	stages := []struct {
		name string
		run  func() bool
	}{
		{"floating IPs", p.floatingIPs},
		{"servers", p.servers},
		{"backups", p.simple("backup", c.ListProjectBackups, c.DeleteBackup)},
		{"snapshots", p.simple("snapshot", c.ListProjectSnapshots, c.DeleteSnapshot)},
		{"volumes", p.simple("volume", c.ListProjectVolumes, c.DeleteVolume)},
		{"images", p.simple("image", c.ListProjectImages, c.DeleteImage)},
		{"routers", p.routers},
		{"ports", p.ports},
		{"networks", p.simple("network", c.ListProjectNetworks, c.DeleteNetwork)},
		{"security groups", p.securityGroups},
	}
	for _, s := range stages {
		if !s.run() {
			log.Infow("Purge: waiting for a stage to empty", "stage", s.name, "removed", p.removed, "dry_run", dryRun)
			return purgeResult{stage: s.name, removed: p.removed}
		}
	}
	if dryRun {
		log.Infow("Dry run: would delete the emptied project")
		return purgeResult{stage: "project"}
	}
	if err := c.DeleteProject(projectID); err != nil {
		log.Warnw("Purge: could not delete the emptied project", "error", err)
		return purgeResult{stage: "project", removed: p.removed}
	}
	log.Infow("Purge: project deleted")
	return purgeResult{deleted: true, removed: p.removed}
}

type purger struct {
	c         purgeClient
	projectID string
	dryRun    bool
	log       *zap.SugaredLogger
	removed   int
}

// owned keeps what belongs to the project, and says so loudly about anything
// else: a service that ignored the project filter must not turn into deleting
// somebody else's resources.
func (p *purger) owned(kind string, items []osclient.Resource) []osclient.Resource {
	var out []osclient.Resource
	for _, it := range items {
		if it.ProjectID != p.projectID {
			p.log.Errorw("Purge: listing returned a resource of another project — skipped", "kind", kind, "id", it.ID, "its_project_id", it.ProjectID)
			continue
		}
		out = append(out, it)
	}
	return out
}

// do runs one deletion unless this is a dry run.
func (p *purger) do(kind, id, name string, fn func() error) {
	if p.dryRun {
		p.log.Infow("Dry run: would delete", "kind", kind, "id", id, "name", name)
		return
	}
	if err := fn(); err != nil {
		p.log.Warnw("Purge: could not delete", "kind", kind, "id", id, "name", name, "error", err)
		return
	}
	p.log.Infow("Purge: deleted", "kind", kind, "id", id, "name", name)
	p.removed++
}

// simple is a stage that lists and deletes one kind of resource.
func (p *purger) simple(kind string, list func(string) ([]osclient.Resource, error), del func(string) error) func() bool {
	return func() bool {
		items, err := list(p.projectID)
		if err != nil {
			p.log.Warnw("Purge: could not list", "kind", kind, "error", err)
			return false
		}
		items = p.owned(kind, items)
		for _, it := range items {
			if inDeletion(it.Status) {
				continue
			}
			p.do(kind, it.ID, it.Name, func() error { return del(it.ID) })
		}
		return len(items) == 0
	}
}

func (p *purger) floatingIPs() bool {
	ips, err := p.c.ListProjectFloatingIPs(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "floating IP", "error", err)
		return false
	}
	items := make([]osclient.Resource, 0, len(ips))
	for _, ip := range ips {
		items = append(items, osclient.Resource{ID: ip.ID, Name: ip.Address, ProjectID: ip.ProjectID})
	}
	items = p.owned("floating IP", items)
	for _, it := range items {
		p.do("floating IP", it.ID, it.Name, func() error { return p.c.ReleaseFloatingIP(it.ID) })
	}
	return len(items) == 0
}

func (p *purger) servers() bool {
	list, err := p.c.ListProjectServers(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "server", "error", err)
		return false
	}
	items := make([]osclient.Resource, 0, len(list))
	for _, s := range list {
		items = append(items, osclient.Resource{ID: s.ID, Name: s.Name, Status: s.Status, ProjectID: s.ProjectID})
	}
	items = p.owned("server", items)
	for _, it := range items {
		if it.Status == "DELETED" || it.Status == "SOFT_DELETED" {
			continue
		}
		p.do("server", it.ID, it.Name, func() error { return p.c.DeleteServer(it.ID) })
	}
	return len(items) == 0
}

// routers detaches every router from its subnets first — Neutron refuses to
// delete a router that still has interfaces.
func (p *purger) routers() bool {
	routers, err := p.c.ListProjectRouters(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "router", "error", err)
		return false
	}
	routers = p.owned("router", routers)
	if len(routers) == 0 {
		return true
	}
	ports, err := p.c.ListProjectPorts(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "port", "error", err)
		return false
	}
	for _, rt := range routers {
		for _, port := range ports {
			if port.DeviceID == rt.ID && strings.HasPrefix(port.DeviceOwner, "network:router_interface") {
				p.do("router interface", port.ID, rt.Name, func() error { return p.c.RemoveRouterInterface(rt.ID, port.ID) })
			}
		}
		p.do("router", rt.ID, rt.Name, func() error { return p.c.DeleteRouter(rt.ID) })
	}
	return false
}

// ports deletes what is left once servers and routers are gone — ports made
// by hand, or left over. DHCP ports belong to their network and go with it.
func (p *purger) ports() bool {
	ports, err := p.c.ListProjectPorts(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "port", "error", err)
		return false
	}
	ports = p.owned("port", ports)
	left := 0
	for _, port := range ports {
		if port.DeviceOwner == "network:dhcp" {
			continue
		}
		left++
		p.do("port", port.ID, port.Name, func() error { return p.c.DeletePort(port.ID) })
	}
	return left == 0
}

// securityGroups deletes every group except "default": Neutron creates that
// one again whenever the project's groups are listed, so deleting it would
// never finish, and it holds nothing.
func (p *purger) securityGroups() bool {
	groups, err := p.c.ListProjectSecurityGroups(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "security group", "error", err)
		return false
	}
	groups = p.owned("security group", groups)
	left := 0
	for _, g := range groups {
		if g.Name == "default" {
			continue
		}
		left++
		p.do("security group", g.ID, g.Name, func() error { return p.c.DeleteSecurityGroup(g.ID) })
	}
	return left == 0
}

// inDeletion: already on its way out, asking again only produces an error.
func inDeletion(status string) bool {
	switch strings.ToLower(status) {
	case "deleting", "pending_delete", "deleted":
		return true
	}
	return false
}

// Emptying a project takes a stage per pass, and most stages only finish once
// OpenStack has deleted in the background. At the normal interval that is half
// an hour for an ordinary project, so while one is being emptied the next pass
// comes after followUpDelay instead. At most maxFollowUps in a row: something
// that cannot be deleted must not turn into a pass every half minute for good —
// after that the normal interval takes over again.
const (
	followUpDelay = 30 * time.Second
	maxFollowUps  = 20
)

func (r *Reconciler) scheduleFollowUp(purgesPending int) {
	if purgesPending == 0 {
		r.followUps = 0
		return
	}
	if r.followUps >= maxFollowUps {
		return
	}
	r.followUps++
	time.AfterFunc(followUpDelay, r.Trigger)
}
