package reconciler

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
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

	// Access, for the services that only work inside the project.
	ServiceUserID() (string, error)
	ListProjectMembers(projectID string) ([]osclient.ProjectRole, error)
	ListProjectGroupRoles(projectID string) ([]osclient.GroupProjectRole, error)
	RemoveProjectMember(projectID, userID, roleID string) error
	UnassignGroupFromProject(projectID, groupID, roleID string) error
	AddProjectMember(projectID, userID, roleID string) error
	RoleIDByName(name string) (string, error)
	UpdateProject(projectID string, opts osclient.ProjectUpdateOpts) (*projects.Project, error)
}

// projectSession is what the services that only work inside the project
// offer (osclient.ProjectSession); tests substitute a fake.
type projectSession interface {
	ListClusters() ([]osclient.Resource, error)
	DeleteCluster(id string) error
	ListStacks() ([]osclient.Resource, error)
	DeleteStack(name, id string) error
	ListLeases() ([]osclient.Resource, error)
	DeleteLease(id string) error
	ListLoadBalancers() ([]osclient.Resource, error)
	DeleteLoadBalancer(id string) error
	HasDNS() bool
	ListZones() ([]osclient.Resource, error)
	DeleteZone(id string) error
	HasObjectStorage() bool
	ListBuckets() ([]osclient.Resource, error)
	EmptyBucket(name string, limit int) (int, bool, error)
	DeleteBucket(name string) error
	ListSecrets() ([]osclient.Resource, error)
	DeleteSecret(id string) error
	ListSecretContainers() ([]osclient.Resource, error)
	DeleteSecretContainer(id string) error
	UnemptiedServiceTypes() []string
}

// purgeRole is what the service user is given in a project it empties: Heat,
// Swift and Barbican need a token for the project itself, and deleting what
// others in it created (a colleague's secret, a stack) needs admin there.
const purgeRole = "admin"

// purgeOptions are the switches of one purge.
type purgeOptions struct {
	dryRun bool
	// dnsAndObjects deletes DNS zones and object storage. Off, they are only
	// reported and hold the project back: both exist only on production, so
	// they are switched on after a test there.
	dnsAndObjects bool
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
	open := func(id string) (projectSession, error) { return r.osClient.OpenProjectSession(id) }
	out := purgeProject(r.osClient, open, osProject, scopeParentID,
		purgeOptions{dryRun: r.cfg.DryRun, dnsAndObjects: r.cfg.PurgeDNSAndObjectStorage}, log)
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

func purgeProject(c purgeClient, open func(string) (projectSession, error), project osclient.ProjectInfo, scopeParentID string, o purgeOptions, log *zap.SugaredLogger) purgeResult {
	projectID := project.ID
	if projectID == "" || projectID == scopeParentID {
		log.Errorw("Purge refused: not a project that may be deleted", "scope_parent_id", scopeParentID)
		return purgeResult{stage: "refused"}
	}
	p := purger{c: c, projectID: projectID, dryRun: o.dryRun, log: log}

	// First the way in: nobody else in the project, the service user in it,
	// the project enabled — a token for a disabled project is refused.
	if !p.access(project) {
		log.Infow("Purge: waiting for a stage to empty", "stage", "access", "dry_run", o.dryRun)
		return purgeResult{stage: "access"}
	}
	var s projectSession
	if !o.dryRun {
		var err error
		if s, err = open(projectID); err != nil {
			log.Warnw("Purge: could not sign in to the project, nothing deleted", "error", err)
			return purgeResult{stage: "session"}
		}
		if left := s.UnemptiedServiceTypes(); len(left) > 0 {
			log.Warnw("Purge: the cloud offers services whose resources are not emptied and may be left behind", "service_types", left)
		}
	}
	scoped := func(kind string, list func() ([]osclient.Resource, error), del func(osclient.Resource) error) func() bool {
		return func() bool {
			if s == nil {
				return true // dry run: no session to look with
			}
			return p.scoped(kind, list, del)
		}
	}
	reportOnly := func(kind string, has func() bool, list func() ([]osclient.Resource, error), stage func() bool) func() bool {
		return func() bool {
			if s == nil || !has() {
				return true
			}
			if o.dnsAndObjects {
				return stage()
			}
			items, err := list()
			if err != nil {
				log.Warnw("Purge: could not list", "kind", kind, "error", err)
				return false
			}
			for _, it := range items {
				log.Warnw("Purge: would delete, but deleting this kind is not switched on (RECONCILER_PURGE_DNS_AND_OBJECT_STORAGE) — the project is kept", "kind", kind, "id", it.ID, "name", it.Name)
			}
			return len(items) == 0
		}
	}

	// One stage per pass at most; each returns false while it still has work.
	// What builds on other things goes first: a cluster owns a stack, a stack
	// owns servers and load balancers, a load balancer holds a port.
	stages := []struct {
		name string
		run  func() bool
	}{
		{"clusters", scoped("cluster", func() ([]osclient.Resource, error) { return s.ListClusters() },
			func(r osclient.Resource) error { return s.DeleteCluster(r.ID) })},
		{"stacks", scoped("stack", func() ([]osclient.Resource, error) { return s.ListStacks() },
			func(r osclient.Resource) error { return s.DeleteStack(r.Name, r.ID) })},
		{"leases", scoped("lease", func() ([]osclient.Resource, error) { return s.ListLeases() },
			func(r osclient.Resource) error { return s.DeleteLease(r.ID) })},
		{"load balancers", scoped("load balancer", func() ([]osclient.Resource, error) { return s.ListLoadBalancers() },
			func(r osclient.Resource) error { return s.DeleteLoadBalancer(r.ID) })},
		{"floating IPs", p.floatingIPs},
		{"servers", p.servers},
		{"backups", p.simple("backup", c.ListProjectBackups, c.DeleteBackup)},
		{"snapshots", p.simple("snapshot", c.ListProjectSnapshots, c.DeleteSnapshot)},
		{"volumes", p.simple("volume", c.ListProjectVolumes, c.DeleteVolume)},
		{"images", p.simple("image", c.ListProjectImages, c.DeleteImage)},
		{"DNS zones", reportOnly("DNS zone", func() bool { return s.HasDNS() }, func() ([]osclient.Resource, error) { return s.ListZones() },
			scoped("DNS zone", func() ([]osclient.Resource, error) { return s.ListZones() },
				func(r osclient.Resource) error { return s.DeleteZone(r.ID) }))},
		{"object storage", reportOnly("object storage container", func() bool { return s.HasObjectStorage() },
			func() ([]osclient.Resource, error) { return s.ListBuckets() }, func() bool { return p.buckets(s) })},
		{"secret containers", scoped("secret container", func() ([]osclient.Resource, error) { return s.ListSecretContainers() },
			func(r osclient.Resource) error { return s.DeleteSecretContainer(r.ID) })},
		{"secrets", scoped("secret", func() ([]osclient.Resource, error) { return s.ListSecrets() },
			func(r osclient.Resource) error { return s.DeleteSecret(r.ID) })},
		{"routers", p.routers},
		{"ports", p.ports},
		{"networks", p.simple("network", c.ListProjectNetworks, c.DeleteNetwork)},
		{"security groups", p.securityGroups},
	}
	for _, st := range stages {
		if !st.run() {
			log.Infow("Purge: waiting for a stage to empty", "stage", st.name, "removed", p.removed, "dry_run", o.dryRun)
			return purgeResult{stage: st.name, removed: p.removed}
		}
	}
	if o.dryRun {
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

// access prepares the project for the services that only work inside it:
// every user and group role in it is removed — from here on nobody but the
// service user gets in —, the service user is given purgeRole, and the project
// is enabled again, since a token for a disabled one is refused. Reports
// whether all of that is in place.
func (p *purger) access(project osclient.ProjectInfo) bool {
	self, err := p.c.ServiceUserID()
	if err != nil {
		p.log.Warnw("Purge: could not tell the service user's ID", "error", err)
		return false
	}
	roleID, err := p.c.RoleIDByName(purgeRole)
	if err != nil {
		p.log.Warnw("Purge: could not find the role", "role", purgeRole, "error", err)
		return false
	}
	ready := true

	members, err := p.c.ListProjectMembers(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list the members", "error", err)
		return false
	}
	hasSelf := false
	for _, m := range members {
		if m.UserID == "" {
			continue // a group's, listed below
		}
		if m.UserID == self {
			hasSelf = hasSelf || m.RoleID == roleID
			continue
		}
		ready = false
		p.do("member", m.UserID, m.RoleName, func() error { return p.c.RemoveProjectMember(p.projectID, m.UserID, m.RoleID) })
	}
	groups, err := p.c.ListProjectGroupRoles(p.projectID)
	if err != nil {
		p.log.Warnw("Purge: could not list the group roles", "error", err)
		return false
	}
	for _, g := range groups {
		ready = false
		p.do("group role", g.GroupID, g.RoleName, func() error { return p.c.UnassignGroupFromProject(p.projectID, g.GroupID, g.RoleID) })
	}
	if !hasSelf {
		if p.dryRun {
			p.log.Infow("Dry run: would give the service user a role in the project", "role", purgeRole)
		} else if err := p.c.AddProjectMember(p.projectID, self, roleID); err != nil {
			p.log.Warnw("Purge: could not give the service user a role in the project", "role", purgeRole, "error", err)
			return false
		}
	}
	if !project.Enabled {
		if p.dryRun {
			p.log.Infow("Dry run: would enable the project to sign in to it")
		} else {
			enabled := true
			opts := osclient.ProjectUpdateOpts{}
			opts.Enabled = &enabled
			if _, err := p.c.UpdateProject(p.projectID, opts); err != nil {
				p.log.Warnw("Purge: could not enable the project", "error", err)
				return false
			}
		}
	}
	// Members just removed are checked again next pass before anything is
	// deleted with the project open; the service user's role and the enable
	// count as done once the calls went through.
	return ready || p.dryRun
}

// scoped is a stage over a service of the project session. The session is
// the project's, but a listing is checked against the project all the same.
func (p *purger) scoped(kind string, list func() ([]osclient.Resource, error), del func(osclient.Resource) error) bool {
	items, err := list()
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", kind, "error", err)
		return false
	}
	items = p.owned(kind, items)
	for _, it := range items {
		if inDeletion(it.Status) {
			continue
		}
		p.do(kind, it.ID, it.Name, func() error { return del(it) })
	}
	return len(items) == 0
}

// objectsPerPass bounds how many objects one pass deletes, so a big bucket is
// spread over passes instead of holding one up.
const objectsPerPass = 500

// buckets empties and deletes the project's object storage containers.
func (p *purger) buckets(s projectSession) bool {
	items, err := s.ListBuckets()
	if err != nil {
		p.log.Warnw("Purge: could not list", "kind", "object storage container", "error", err)
		return false
	}
	budget := objectsPerPass
	for _, b := range items {
		if p.dryRun {
			p.log.Infow("Dry run: would empty and delete", "kind", "object storage container", "name", b.Name)
			continue
		}
		if budget <= 0 {
			break
		}
		n, empty, err := s.EmptyBucket(b.Name, budget)
		p.removed += n
		budget -= n
		if err != nil {
			p.log.Warnw("Purge: could not empty", "kind", "object storage container", "name", b.Name, "error", err)
			continue
		}
		if empty {
			p.do("object storage container", b.Name, b.Name, func() error { return s.DeleteBucket(b.Name) })
		}
	}
	return len(items) == 0
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
