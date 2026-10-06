package reconciler

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"go.uber.org/zap"
)

type fakeArchive struct {
	servers     []osclient.ServerInfo
	ips         []osclient.FloatingIPInfo
	shelved     []string
	released    []string
	frozen      bool
	updates     []osclient.ProjectUpdateOpts
	failShelve  string
	failFreeze  bool
	failListIPs bool
}

func (f *fakeArchive) ListProjectServers(string) ([]osclient.ServerInfo, error) {
	return f.servers, nil
}
func (f *fakeArchive) ShelveServer(id string) error {
	if id == f.failShelve {
		return errors.New("nope")
	}
	f.shelved = append(f.shelved, id)
	return nil
}
func (f *fakeArchive) ListProjectFloatingIPs(string) ([]osclient.FloatingIPInfo, error) {
	if f.failListIPs {
		return nil, errors.New("nope")
	}
	return f.ips, nil
}
func (f *fakeArchive) ReleaseFloatingIP(id string) error {
	f.released = append(f.released, id)
	return nil
}
func (f *fakeArchive) FreezeProjectQuotas(string) error {
	if f.failFreeze {
		return errors.New("nope")
	}
	f.frozen = true
	return nil
}
func (f *fakeArchive) UpdateProject(_ string, opts osclient.ProjectUpdateOpts) (*projects.Project, error) {
	f.updates = append(f.updates, opts)
	return &projects.Project{}, nil
}

var archiveDay = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func archive(f *fakeArchive, tags ...string) archiveResult {
	p := osclient.ProjectInfo{ID: "os-1", Enabled: true, Tags: append([]string{"managed", "status:released"}, tags...)}
	return archiveProject(f, p, "archived:", false, archiveDay, zap.NewNop().Sugar())
}

// Everything that frees something: servers shelved (those that can be), IPs
// released, quotas frozen, and then the project disabled and marked in one go,
// its other tags kept.
func TestArchive_FreesWhatCanBeFreed(t *testing.T) {
	f := &fakeArchive{
		servers: []osclient.ServerInfo{
			{ID: "s-active", Status: "ACTIVE", ProjectID: "os-1"}, {ID: "s-off", Status: "SHUTOFF", ProjectID: "os-1"},
			{ID: "s-done", Status: "SHELVED_OFFLOADED", ProjectID: "os-1"}, {ID: "s-broken", Status: "ERROR", ProjectID: "os-1"},
		},
		ips: []osclient.FloatingIPInfo{{ID: "ip-1", Address: "141.72.0.1", ProjectID: "os-1"}},
	}
	out := archive(f)

	if !slices.Equal(f.shelved, []string{"s-active", "s-off"}) {
		t.Errorf("shelved %v, want the running and the stopped one — not one already shelved, not one in ERROR", f.shelved)
	}
	if !slices.Equal(f.released, []string{"ip-1"}) || !f.frozen {
		t.Errorf("released %v, frozen %v", f.released, f.frozen)
	}
	if !out.archived || out.shelved != 2 || out.ipsReleased != 1 {
		t.Errorf("result = %+v", out)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want one project update, got %d", len(f.updates))
	}
	u := f.updates[0]
	if u.Enabled == nil || *u.Enabled {
		t.Error("the project must be disabled")
	}
	if !slices.Equal(*u.Tags, []string{"managed", "status:released", "archived:2026-10-05"}) {
		t.Errorf("tags = %v, want the old ones plus the archive mark", *u.Tags)
	}
}

// A marked project is left alone — an admin who restored it by hand is not
// undone on the next pass.
func TestArchive_LeavesAMarkedProjectAlone(t *testing.T) {
	f := &fakeArchive{servers: []osclient.ServerInfo{{ID: "s-1", Status: "ACTIVE", ProjectID: "os-1"}}}
	if out := archive(f, "archived:2026-09-01"); out.archived || len(f.shelved) != 0 || len(f.updates) != 0 || f.frozen {
		t.Errorf("an archived project was touched: %+v, %+v", out, f)
	}
}

// A step that fails leaves the mark off, so the next pass tries again; what
// did go through is not undone.
func TestArchive_RetriesUntilComplete(t *testing.T) {
	for name, f := range map[string]*fakeArchive{
		"shelve fails":  {servers: []osclient.ServerInfo{{ID: "s-1", Status: "ACTIVE", ProjectID: "os-1"}, {ID: "s-2", Status: "ACTIVE", ProjectID: "os-1"}}, failShelve: "s-1"},
		"quota fails":   {failFreeze: true},
		"ip list fails": {failListIPs: true},
	} {
		t.Run(name, func(t *testing.T) {
			out := archive(f)
			if out.archived || len(f.updates) != 0 {
				t.Errorf("marked as archived although a step failed: %+v", out)
			}
		})
	}
}

// A dry run reports and touches nothing.
func TestArchive_DryRun(t *testing.T) {
	f := &fakeArchive{servers: []osclient.ServerInfo{{ID: "s-1", Status: "ACTIVE", ProjectID: "os-1"}}}
	p := osclient.ProjectInfo{ID: "os-1", Enabled: true}
	if out := archiveProject(f, p, "archived:", true, archiveDay, zap.NewNop().Sugar()); out.archived || len(f.shelved) != 0 || f.frozen {
		t.Errorf("dry run changed something: %+v", f)
	}
}

func TestValidateReleasedDelete(t *testing.T) {
	for _, ok := range []string{"", "never", "on-request", "after-grace", "immediately"} {
		if err := ValidateReleasedDelete(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	if err := ValidateReleasedDelete("sometimes"); err == nil {
		t.Error(`"sometimes" accepted`)
	}
}

// A server or IP the listing returns for another project is never touched.
func TestArchive_SkipsWhatIsNotTheProjects(t *testing.T) {
	f := &fakeArchive{
		servers: []osclient.ServerInfo{{ID: "s-other", Status: "ACTIVE", ProjectID: "os-2"}},
		ips:     []osclient.FloatingIPInfo{{ID: "ip-other", ProjectID: "os-2"}},
	}
	archive(f)
	if len(f.shelved) != 0 || len(f.released) != 0 {
		t.Errorf("touched another project's resources: shelved %v, released %v", f.shelved, f.released)
	}
}
