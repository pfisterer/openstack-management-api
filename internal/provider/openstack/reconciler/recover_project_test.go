package reconciler

import (
	"context"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// fakeTagReader reads the resource-id tag the way the real client does, without
// needing a cloud behind it.
type fakeTagReader struct{ prefix string }

func (f fakeTagReader) ExtractResourceIDFromTags(tags []string) string {
	for _, tag := range tags {
		if rest, ok := strings.CutPrefix(tag, f.prefix); ok {
			return rest
		}
	}
	return ""
}

// deletedStore records what the recovery removed, so a test can tell "kept" from
// "deleted" without a database. It has no nodes to check the guards against, so
// every guarded delete counts as allowed.
type deletedStore struct {
	TreeSync
	deleted []string
}

func (s *deletedStore) RemoveImport(_ context.Context, id string) (bool, error) {
	s.deleted = append(s.deleted, id)
	return true, nil
}

func (s *deletedStore) RemoveRetired(_ context.Context, id string) (bool, error) {
	s.deleted = append(s.deleted, id)
	return true, nil
}

const testTagPrefix = "managed-resource-id:"

func testTagReader() fakeTagReader { return fakeTagReader{prefix: testTagPrefix} }

func project(id string, tags ...string) osclient.ProjectInfo {
	return osclient.ProjectInfo{ID: id, Name: "project " + id, Tags: tags}
}

// The whole point: a project that lost its tag must be recognised by the ID the
// leaf remembers, not treated as missing.
func TestChooseRecoverableProject(t *testing.T) {
	log := zap.NewNop().Sugar()
	leaf := model.Node{ID: "p_001", Bindings: boundTo("os-1")}

	tests := []struct {
		name    string
		leaf    model.Node
		byOSID  map[string]osclient.ProjectInfo
		claimed map[string]string
		want    bool
	}{
		{
			name:   "untagged project is reclaimed",
			leaf:   leaf,
			byOSID: map[string]osclient.ProjectInfo{"os-1": project("os-1")},
			want:   true,
		},
		{
			name:   "a project still carrying our own tag is ours too",
			leaf:   leaf,
			byOSID: map[string]osclient.ProjectInfo{"os-1": project("os-1", testTagPrefix+"p_001")},
			want:   true,
		},
		{
			name:   "a leaf that never had a project gets none",
			leaf:   model.Node{ID: "p_001"},
			byOSID: map[string]osclient.ProjectInfo{"os-1": project("os-1")},
			want:   false,
		},
		{
			name:   "a project that is really gone stays gone",
			leaf:   leaf,
			byOSID: map[string]osclient.ProjectInfo{},
			want:   false,
		},
		{
			// The stored ID is stale — reclaiming would steal the project from
			// whichever node holds it now.
			name:   "a project tagged for another node is left alone",
			leaf:   leaf,
			byOSID: map[string]osclient.ProjectInfo{"os-1": project("os-1", testTagPrefix+"p_999")},
			want:   false,
		},
		{
			name:    "a project already claimed this run is not handed out twice",
			leaf:    leaf,
			byOSID:  map[string]osclient.ProjectInfo{"os-1": project("os-1")},
			claimed: map[string]string{"os-1": "p_other"},
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claimed := tc.claimed
			if claimed == nil {
				claimed = map[string]string{}
			}
			got, ok := chooseRecoverableProject(testTagReader(), tc.leaf, tc.byOSID, claimed, log)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && got.ID != osID(tc.leaf) {
				t.Errorf("recovered the wrong project: %q", got.ID)
			}
		})
	}
}

// newRecoveryReconciler builds a reconciler whose OpenStack side is never
// touched: dry-run skips the tag write, which is the only call that would need a
// live client.
func newRecoveryReconciler(nodes TreeSync, cfg Config) *Reconciler {
	cfg.DryRun = true
	client := &osclient.OpenStackClient{}
	client.SetTagConfig("managed", testTagPrefix)
	return &Reconciler{nodes: nodes, osClient: client, cfg: cfg, log: zap.NewNop().Sugar()}
}

// The recovered project must disappear from the import phase's view, or the same
// project would be imported a second time as an unmanaged one in the same run.
func TestRecoverUntaggedProjectHidesItFromTheImportPhase(t *testing.T) {
	store := &deletedStore{}
	r := newRecoveryReconciler(store, Config{})
	leaf := model.Node{ID: "p_001", Bindings: boundTo("os-1")}
	byOSID := map[string]osclient.ProjectInfo{"os-1": project("os-1")}
	res := reconcileResult{}

	got, ok := r.recoverUntaggedProject(context.Background(), leaf, byOSID,
		map[string]model.Node{}, map[string]string{}, &res)
	if !ok || got.ID != "os-1" {
		t.Fatalf("expected to recover os-1, got %q (ok=%v)", got.ID, ok)
	}
	if _, still := byOSID["os-1"]; still {
		t.Error("the recovered project is still visible to the import phase")
	}
	if res.projectsRetagged != 1 {
		t.Errorf("projectsRetagged = %d, want 1", res.projectsRetagged)
	}
}

// An earlier tick imported the untagged project as an "imported" leaf. Once the
// managed leaf reclaims it, that import describes the same OpenStack project and
// has to go — otherwise a manager can adopt a project that is already managed.
func TestRecoverUntaggedProjectRemovesTheShadowingImport(t *testing.T) {
	store := &deletedStore{}
	r := newRecoveryReconciler(store, Config{})
	leaf := model.Node{ID: "p_001", Bindings: boundTo("os-1")}
	imported := map[string]model.Node{"os-1": {ID: "p_shadow", Bindings: boundTo("os-1"), Status: model.StatusImported}}
	res := reconcileResult{}

	if _, ok := r.recoverUntaggedProject(context.Background(), leaf,
		map[string]osclient.ProjectInfo{"os-1": project("os-1")},
		imported, map[string]string{}, &res); !ok {
		t.Fatal("expected the project to be recovered")
	}
	if _, still := imported["os-1"]; still {
		t.Error("the shadowing import is still in the map and would be treated as stale")
	}
	if res.importedRemoved != 1 {
		t.Errorf("importedRemoved = %d, want 1", res.importedRemoved)
	}
}
