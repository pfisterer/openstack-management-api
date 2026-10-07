package reconciler

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// fakeCloud holds one project's resources; deleting removes them at once,
// which is what the next pass would see.
type fakeCloud struct {
	servers  []osclient.ServerInfo
	ips      []osclient.FloatingIPInfo
	res      map[string][]osclient.Resource // backups, snapshots, volumes, images, routers, ports, networks, sgs
	deleted  []string
	project  bool // project deleted
	fail     map[string]bool
	detached []string

	// Access: role assignments in the project and whether it is enabled.
	members  []osclient.ProjectRole
	groups   []osclient.GroupProjectRole
	enabled  bool
	selfRole bool
	// session is what signing in to the project finds.
	session *fakeSession
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{res: map[string][]osclient.Resource{}, session: newFakeSession(nil)}
}

func (f *fakeCloud) ServiceUserID() (string, error) { return "svc", nil }
func (f *fakeCloud) ListProjectMembers(string) ([]osclient.ProjectRole, error) {
	out := slices.Clone(f.members)
	if f.selfRole {
		out = append(out, osclient.ProjectRole{UserID: "svc", RoleID: "r-admin"})
	}
	return out, nil
}
func (f *fakeCloud) ListProjectGroupRoles(string) ([]osclient.GroupProjectRole, error) {
	return slices.Clone(f.groups), nil
}
func (f *fakeCloud) RemoveProjectMember(_, userID, _ string) error {
	f.members = slices.DeleteFunc(f.members, func(m osclient.ProjectRole) bool { return m.UserID == userID })
	f.deleted = append(f.deleted, "member:"+userID)
	return nil
}
func (f *fakeCloud) UnassignGroupFromProject(_, groupID, _ string) error {
	f.groups = slices.DeleteFunc(f.groups, func(g osclient.GroupProjectRole) bool { return g.GroupID == groupID })
	f.deleted = append(f.deleted, "group:"+groupID)
	return nil
}
func (f *fakeCloud) AddProjectMember(_, userID, roleID string) error {
	if userID == "svc" && roleID == "r-admin" {
		f.selfRole = true
	}
	return nil
}
func (f *fakeCloud) RoleIDByName(string) (string, error) { return "r-admin", nil }
func (f *fakeCloud) UpdateProject(_ string, opts osclient.ProjectUpdateOpts) (*projects.Project, error) {
	if opts.Enabled != nil {
		f.enabled = *opts.Enabled
	}
	return &projects.Project{}, nil
}
func (f *fakeCloud) open(string) (projectSession, error) {
	if !f.enabled || !f.selfRole {
		return nil, errors.New("not allowed in")
	}
	return f.session, nil
}

// fakeSession holds the resources only reachable from inside the project.
type fakeSession struct {
	res        map[string][]osclient.Resource // cluster, stack, lease, lb, zone, bucket, secret, secretct
	objects    map[string]int
	hasDNS     bool
	hasObjects bool
	deleted    *[]string
	unemptied  []string
}

func newFakeSession(deleted *[]string) *fakeSession {
	return &fakeSession{res: map[string][]osclient.Resource{}, objects: map[string]int{}, deleted: deleted}
}
func (s *fakeSession) list(kind string) ([]osclient.Resource, error) {
	return slices.Clone(s.res[kind]), nil
}
func (s *fakeSession) remove(kind, id string) error {
	s.res[kind] = slices.DeleteFunc(s.res[kind], func(r osclient.Resource) bool { return r.ID == id })
	if s.deleted != nil {
		*s.deleted = append(*s.deleted, kind+":"+id)
	}
	return nil
}
func (s *fakeSession) ListClusters() ([]osclient.Resource, error)         { return s.list("cluster") }
func (s *fakeSession) DeleteCluster(id string) error                      { return s.remove("cluster", id) }
func (s *fakeSession) ListStacks() ([]osclient.Resource, error)           { return s.list("stack") }
func (s *fakeSession) DeleteStack(_, id string) error                     { return s.remove("stack", id) }
func (s *fakeSession) ListLeases() ([]osclient.Resource, error)           { return s.list("lease") }
func (s *fakeSession) DeleteLease(id string) error                        { return s.remove("lease", id) }
func (s *fakeSession) ListLoadBalancers() ([]osclient.Resource, error)    { return s.list("lb") }
func (s *fakeSession) DeleteLoadBalancer(id string) error                 { return s.remove("lb", id) }
func (s *fakeSession) HasDNS() bool                                       { return s.hasDNS }
func (s *fakeSession) ListZones() ([]osclient.Resource, error)            { return s.list("zone") }
func (s *fakeSession) DeleteZone(id string) error                         { return s.remove("zone", id) }
func (s *fakeSession) HasObjectStorage() bool                             { return s.hasObjects }
func (s *fakeSession) ListBuckets() ([]osclient.Resource, error)          { return s.list("bucket") }
func (s *fakeSession) DeleteBucket(name string) error                     { return s.remove("bucket", name) }
func (s *fakeSession) ListSecrets() ([]osclient.Resource, error)          { return s.list("secret") }
func (s *fakeSession) DeleteSecret(id string) error                       { return s.remove("secret", id) }
func (s *fakeSession) ListSecretContainers() ([]osclient.Resource, error) { return s.list("secretct") }
func (s *fakeSession) DeleteSecretContainer(id string) error              { return s.remove("secretct", id) }
func (s *fakeSession) UnemptiedServiceTypes() []string                    { return s.unemptied }
func (s *fakeSession) EmptyBucket(name string, limit int) (int, bool, error) {
	n := min(s.objects[name], limit)
	s.objects[name] -= n
	return n, s.objects[name] == 0, nil
}

func (f *fakeCloud) remove(kind, id string) error {
	if f.fail[id] {
		return errors.New("nope")
	}
	f.res[kind] = slices.DeleteFunc(f.res[kind], func(r osclient.Resource) bool { return r.ID == id })
	f.deleted = append(f.deleted, kind+":"+id)
	return nil
}
func (f *fakeCloud) list(kind string) ([]osclient.Resource, error) {
	return slices.Clone(f.res[kind]), nil
}

func (f *fakeCloud) ListProjectServers(string) ([]osclient.ServerInfo, error) {
	return slices.Clone(f.servers), nil
}
func (f *fakeCloud) DeleteServer(id string) error {
	f.servers = slices.DeleteFunc(f.servers, func(s osclient.ServerInfo) bool { return s.ID == id })
	f.deleted = append(f.deleted, "server:"+id)
	return nil
}
func (f *fakeCloud) ListProjectFloatingIPs(string) ([]osclient.FloatingIPInfo, error) {
	return slices.Clone(f.ips), nil
}
func (f *fakeCloud) ReleaseFloatingIP(id string) error {
	f.ips = slices.DeleteFunc(f.ips, func(i osclient.FloatingIPInfo) bool { return i.ID == id })
	f.deleted = append(f.deleted, "ip:"+id)
	return nil
}
func (f *fakeCloud) ListProjectBackups(string) ([]osclient.Resource, error) { return f.list("backup") }
func (f *fakeCloud) DeleteBackup(id string) error                           { return f.remove("backup", id) }
func (f *fakeCloud) ListProjectSnapshots(string) ([]osclient.Resource, error) {
	return f.list("snapshot")
}
func (f *fakeCloud) DeleteSnapshot(id string) error                         { return f.remove("snapshot", id) }
func (f *fakeCloud) ListProjectVolumes(string) ([]osclient.Resource, error) { return f.list("volume") }
func (f *fakeCloud) DeleteVolume(id string) error                           { return f.remove("volume", id) }
func (f *fakeCloud) ListProjectImages(string) ([]osclient.Resource, error)  { return f.list("image") }
func (f *fakeCloud) DeleteImage(id string) error                            { return f.remove("image", id) }
func (f *fakeCloud) ListProjectRouters(string) ([]osclient.Resource, error) { return f.list("router") }
func (f *fakeCloud) RemoveRouterInterface(routerID, portID string) error {
	f.detached = append(f.detached, routerID+"/"+portID)
	return f.remove("port", portID)
}
func (f *fakeCloud) DeleteRouter(id string) error {
	for _, p := range f.res["port"] {
		if p.DeviceID == id {
			return errors.New("router still has interfaces")
		}
	}
	return f.remove("router", id)
}
func (f *fakeCloud) ListProjectPorts(string) ([]osclient.Resource, error) { return f.list("port") }
func (f *fakeCloud) DeletePort(id string) error                           { return f.remove("port", id) }
func (f *fakeCloud) ListProjectNetworks(string) ([]osclient.Resource, error) {
	return f.list("network")
}
func (f *fakeCloud) DeleteNetwork(id string) error {
	f.res["port"] = slices.DeleteFunc(f.res["port"], func(p osclient.Resource) bool { return p.DeviceOwner == "network:dhcp" })
	return f.remove("network", id)
}
func (f *fakeCloud) ListProjectSecurityGroups(string) ([]osclient.Resource, error) {
	return f.list("sg")
}
func (f *fakeCloud) DeleteSecurityGroup(id string) error { return f.remove("sg", id) }
func (f *fakeCloud) DeleteProject(string) error {
	f.project = true
	return nil
}

const pid = "os-1"

func fullProject() *fakeCloud {
	f := newFakeCloud()
	f.servers = []osclient.ServerInfo{{ID: "vm", Status: "SHELVED_OFFLOADED", ProjectID: pid}}
	f.ips = []osclient.FloatingIPInfo{{ID: "fip", ProjectID: pid}}
	own := func(id string) osclient.Resource { return osclient.Resource{ID: id, ProjectID: pid} }
	f.res["backup"] = []osclient.Resource{own("bak")}
	f.res["snapshot"] = []osclient.Resource{own("snap")}
	f.res["volume"] = []osclient.Resource{own("vol")}
	f.res["image"] = []osclient.Resource{own("shelve-img")}
	f.res["router"] = []osclient.Resource{own("rt")}
	f.res["port"] = []osclient.Resource{
		{ID: "rt-if", ProjectID: pid, DeviceOwner: "network:router_interface", DeviceID: "rt"},
		{ID: "dhcp", ProjectID: pid, DeviceOwner: "network:dhcp"},
		{ID: "loose", ProjectID: pid},
	}
	f.res["network"] = []osclient.Resource{own("net")}
	f.res["sg"] = []osclient.Resource{{ID: "sg-default", Name: "default", ProjectID: pid}, {ID: "sg-web", Name: "web", ProjectID: pid}}
	f.session.deleted = &f.deleted
	return f
}

// purge runs one pass the way the reconciler does: with the project as
// OpenStack shows it now.
func purge(f *fakeCloud, o purgeOptions) purgeResult {
	return purgeProject(f, f.open, osclient.ProjectInfo{ID: pid, Enabled: f.enabled}, "scope", o, zap.NewNop().Sugar())
}

func purgePasses(t *testing.T, f *fakeCloud, max int) int {
	t.Helper()
	for pass := 1; pass <= max; pass++ {
		if out := purge(f, purgeOptions{}); out.deleted {
			return pass
		}
	}
	return 0
}

// Everything goes, a stage per pass, in an order OpenStack accepts; only then
// the project itself.
func TestPurge_EmptiesThenDeletes(t *testing.T) {
	f := fullProject()
	passes := purgePasses(t, f, 20)
	if passes == 0 {
		t.Fatalf("project never deleted; deleted so far: %v", f.deleted)
	}
	want := []string{"ip:fip", "server:vm", "backup:bak", "snapshot:snap", "volume:vol", "image:shelve-img",
		"port:rt-if", "router:rt", "port:loose", "network:net", "sg:sg-web"}
	if !slices.Equal(f.deleted, want) {
		t.Errorf("deleted in order\n%v\nwant\n%v", f.deleted, want)
	}
	if !f.project {
		t.Error("the project itself was not deleted")
	}
}

// Something that cannot be deleted holds the project back — it is never
// deleted with things left in it.
func TestPurge_StuckResourceKeepsTheProject(t *testing.T) {
	f := fullProject()
	f.fail = map[string]bool{"vol": true}
	if purgePasses(t, f, 20) != 0 || f.project {
		t.Fatal("project deleted although a volume could not be")
	}
	if slices.Contains(f.deleted, "image:shelve-img") {
		t.Error("later stages ran while an earlier one was not empty")
	}
}

// A listing that returns another project's resources does not lead to
// deleting them: they are skipped and named in the log.
func TestPurge_NeverTouchesAnotherProject(t *testing.T) {
	f := fullProject()
	f.res["volume"] = append(f.res["volume"], osclient.Resource{ID: "foreign", ProjectID: "os-2"})
	purgePasses(t, f, 20)
	if slices.Contains(f.deleted, "volume:foreign") {
		t.Fatal("deleted another project's volume")
	}
}

// The scope parent and an empty ID are refused outright.
func TestPurge_RefusesTheScopeParent(t *testing.T) {
	for _, id := range []string{"", "scope"} {
		f := fullProject()
		out := purgeProject(f, f.open, osclient.ProjectInfo{ID: id}, "scope", purgeOptions{}, zap.NewNop().Sugar())
		if out.deleted || len(f.deleted) != 0 || f.project {
			t.Errorf("project %q: purge went ahead: %v", id, f.deleted)
		}
	}
}

func TestPurge_DryRunTouchesNothing(t *testing.T) {
	f := fullProject()
	for range 5 {
		purge(f, purgeOptions{dryRun: true})
	}
	if len(f.deleted) != 0 || f.project {
		t.Errorf("dry run deleted %v", f.deleted)
	}
}

func TestDeletionDue(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	plain := tree.Node{}
	asked := tree.Node{Flags: []string{tree.FlagDeleteRequested}}
	overdue := []string{"pending-deletion:2026-10-06"}
	notYet := []string{"pending-deletion:2026-10-07"}

	for _, c := range []struct {
		mode string
		leaf tree.Node
		tags []string
		want bool
	}{
		{"never", asked, overdue, false},
		{"on-request", plain, overdue, false},
		{"on-request", asked, nil, true},
		{"after-grace", plain, notYet, false},
		{"after-grace", plain, overdue, true},
		{"after-grace", asked, notYet, true},
		{"immediately", plain, nil, true},
	} {
		if got := deletionDue(c.mode, c.leaf, c.tags, "pending-deletion:", now); got != c.want {
			t.Errorf("%s, flags %v, tags %v: %v, want %v", c.mode, c.leaf.Flags, c.tags, got, c.want)
		}
	}
}

// While a project is being emptied the next pass comes early, but not for
// ever: after maxFollowUps in a row the normal interval takes over.
func TestScheduleFollowUp(t *testing.T) {
	r := &Reconciler{trigger: make(chan struct{}, 1)}
	for range maxFollowUps + 5 {
		r.scheduleFollowUp(1)
	}
	if r.followUps != maxFollowUps {
		t.Errorf("follow-ups = %d, want the cap %d", r.followUps, maxFollowUps)
	}
	r.scheduleFollowUp(0)
	if r.followUps != 0 {
		t.Error("nothing pending must reset the count")
	}
}

// Before anything inside the project is touched, nobody else is left in it,
// the service user is, and the project is enabled to sign in to.
func TestPurge_AccessFirst(t *testing.T) {
	f := fullProject()
	f.members = []osclient.ProjectRole{{UserID: "alice", RoleID: "r-member", RoleName: "member"}}
	f.groups = []osclient.GroupProjectRole{{GroupID: "course", RoleID: "r-member"}}

	if out := purge(f, purgeOptions{}); out.stage != "access" {
		t.Fatalf("first pass stage %q, want access", out.stage)
	}
	if !slices.Equal(f.deleted, []string{"member:alice", "group:course"}) {
		t.Errorf("first pass deleted %v, want only the access", f.deleted)
	}
	if !f.selfRole || !f.enabled {
		t.Errorf("service user role %v, enabled %v", f.selfRole, f.enabled)
	}
	if purgePasses(t, f, 30) == 0 {
		t.Fatalf("project never deleted: %v", f.deleted)
	}
}

// The services reached from inside the project go first, in the order they
// build on each other.
func TestPurge_ScopedServicesFirst(t *testing.T) {
	f := fullProject()
	own := func(id string) osclient.Resource { return osclient.Resource{ID: id, ProjectID: pid} }
	f.session.res["cluster"] = []osclient.Resource{own("k8s")}
	f.session.res["stack"] = []osclient.Resource{own("stk")}
	f.session.res["lease"] = []osclient.Resource{own("lease")}
	f.session.res["lb"] = []osclient.Resource{own("lb")}
	f.session.res["secret"] = []osclient.Resource{own("sec")}
	f.session.res["secretct"] = []osclient.Resource{own("cert")}
	if purgePasses(t, f, 40) == 0 {
		t.Fatalf("project never deleted: %v", f.deleted)
	}
	want := []string{"cluster:k8s", "stack:stk", "lease:lease", "lb:lb", "ip:fip", "server:vm", "backup:bak", "snapshot:snap",
		"volume:vol", "image:shelve-img", "secretct:cert", "secret:sec", "port:rt-if", "router:rt", "port:loose", "network:net", "sg:sg-web"}
	if !slices.Equal(f.deleted, want) {
		t.Errorf("deleted in order\n%v\nwant\n%v", f.deleted, want)
	}
}

// DNS zones and object storage are only reported while their switch is off,
// and keep the project; with it on they go, buckets emptied first.
func TestPurge_DNSAndObjectsNeedTheSwitch(t *testing.T) {
	f := fullProject()
	f.session.hasDNS, f.session.hasObjects = true, true
	f.session.res["zone"] = []osclient.Resource{{ID: "z", Name: "x.example.", ProjectID: pid}}
	f.session.res["bucket"] = []osclient.Resource{{ID: "data", Name: "data", ProjectID: pid}}
	f.session.objects["data"] = objectsPerPass + 10

	for range 30 {
		if purge(f, purgeOptions{}).deleted {
			t.Fatal("project deleted with a zone and a bucket left")
		}
	}
	if slices.Contains(f.deleted, "zone:z") || f.session.objects["data"] != objectsPerPass+10 {
		t.Fatal("deleted DNS or objects although switched off")
	}
	// The zone goes first, then the bucket a pass's worth of objects at a time.
	if purge(f, purgeOptions{dnsAndObjects: true}); !slices.Contains(f.deleted, "zone:z") || f.session.objects["data"] != objectsPerPass+10 {
		t.Errorf("first pass: zone deleted %v, %d objects left", slices.Contains(f.deleted, "zone:z"), f.session.objects["data"])
	}
	if purge(f, purgeOptions{dnsAndObjects: true}); f.session.objects["data"] != 10 {
		t.Errorf("second pass left %d objects, want one pass worth deleted", f.session.objects["data"])
	}
	for range 10 {
		if purge(f, purgeOptions{dnsAndObjects: true}).deleted {
			break
		}
	}
	if !f.project || !slices.Contains(f.deleted, "zone:z") || !slices.Contains(f.deleted, "bucket:data") {
		t.Errorf("with the switch on: project deleted %v, deleted %v", f.project, f.deleted)
	}
}

// A service-user session that cannot be opened stops everything.
func TestPurge_NoSessionNoDeleting(t *testing.T) {
	f := fullProject()
	out := purgeProject(f, func(string) (projectSession, error) { return nil, errors.New("app credential") },
		osclient.ProjectInfo{ID: pid}, "scope", purgeOptions{}, zap.NewNop().Sugar())
	if out.stage != "session" || len(f.deleted) != 0 {
		t.Errorf("stage %q, deleted %v", out.stage, f.deleted)
	}
}
