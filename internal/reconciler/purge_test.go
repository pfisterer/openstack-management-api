package reconciler

import (
	"errors"
	"slices"
	"testing"
	"time"

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
}

func newFakeCloud() *fakeCloud { return &fakeCloud{res: map[string][]osclient.Resource{}} }

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
	return f
}

func purgePasses(t *testing.T, f *fakeCloud, max int) int {
	t.Helper()
	for pass := 1; pass <= max; pass++ {
		if out := purgeProject(f, pid, "scope", false, zap.NewNop().Sugar()); out.deleted {
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
		out := purgeProject(f, id, "scope", false, zap.NewNop().Sugar())
		if out.deleted || len(f.deleted) != 0 || f.project {
			t.Errorf("project %q: purge went ahead: %v", id, f.deleted)
		}
	}
}

func TestPurge_DryRunTouchesNothing(t *testing.T) {
	f := fullProject()
	for range 5 {
		purgeProject(f, pid, "scope", true, zap.NewNop().Sugar())
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
