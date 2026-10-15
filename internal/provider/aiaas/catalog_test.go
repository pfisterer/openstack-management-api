package aiaas

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

type fakeCatalogService struct {
	classes []GPUClass
	err     error
}

func (f *fakeCatalogService) Catalog(context.Context) ([]GPUClass, error) { return f.classes, f.err }

func TestGPUCatalog(t *testing.T) {
	ctx := context.Background()
	static := []common.Resource{{ID: "ws-gpus-l4-3q", Name: "GPUs L4 (3Q) at once", Provider: Provider}, {ID: "llm-mtokens", Provider: Provider}}
	svc := &fakeCatalogService{classes: []GPUClass{{ID: "l4-3q", Name: "Allgemein · klein"}, {ID: "allgemein-mittel", Name: "Allgemein · mittel", Pool: "allgemein", PoolName: "Allgemein", Tier: "mittel", TierName: "mittel", Products: "NVIDIA-L40S-12Q", Lends: true}}}
	store := &MemoryClassStore{}
	g := NewGPUCatalog(static, store, svc, zap.NewNop().Sugar())
	adopted := 0
	g.OnNew = func(context.Context) error { adopted++; return nil }

	if added, err := g.Refresh(ctx); err != nil || !added || adopted != 1 {
		t.Fatalf("first refresh: %v %v %d", added, err, adopted)
	}
	ids := map[string]common.Resource{}
	for _, r := range g.Resources() {
		ids[r.ID] = r
	}
	// The configured resource stays as configured; the rest comes from the classes.
	if ids["ws-gpus-l4-3q"].Name != "GPUs L4 (3Q) at once" || ids["ws-gpu-hours-l4-3q"].Kind != common.KindWindow || ids["ws-gpus-allgemein-mittel"].Name != "Allgemein · mittel: GPUs at the same time" || len(ids) != 5 {
		t.Errorf("resources: %v", ids)
	}
	// The UI arranges them by pool.
	if c := ids["ws-gpu-hours-allgemein-mittel"].GPUClass; c == nil || c.PoolName != "Allgemein" || c.Tier != "mittel" || c.Products != "NVIDIA-L40S-12Q" || !c.Lends || ids["ws-gpus-l4-3q"].GPUClass != nil {
		t.Errorf("class info: %+v", c)
	}
	// Nothing new: nothing adopted. The AI service down: the classes seen stay.
	if added, _ := g.Refresh(ctx); added || adopted != 1 {
		t.Errorf("second refresh adopted again")
	}
	svc.err = errors.New("down")
	if _, err := g.Refresh(ctx); err == nil || len(g.Resources()) != 5 {
		t.Errorf("classes lost when the AI service is down")
	}
	// A new start remembers them.
	again := NewGPUCatalog(static, store, svc, zap.NewNop().Sugar())
	if err := again.Load(ctx); err != nil || len(again.Resources()) != 5 {
		t.Errorf("classes not kept: %v %d", err, len(again.Resources()))
	}
	// A class that replaced one from before pools: the tree renames once.
	svc.err = nil
	svc.classes = append(svc.classes, GPUClass{ID: "allgemein-gross", Name: "Allgemein · groß", Replaces: "h100"})
	var renames []map[string]string
	g.OnRename = func(_ context.Context, r map[string]string) error { renames = append(renames, r); return nil }
	_, _ = g.Refresh(ctx)
	_, _ = g.Refresh(ctx)
	if len(renames) != 1 || renames[0]["ws-gpus-h100"] != "ws-gpus-allgemein-gross" || renames[0]["ws-gpu-hours-h100"] != "ws-gpu-hours-allgemein-gross" {
		t.Errorf("renames: %v", renames)
	}
	// A class the AI service no longer lists stays (projects hold it).
	svc.err, svc.classes = nil, []GPUClass{{ID: "l4-3q", Name: "Allgemein · klein"}}
	if _, err := g.Refresh(ctx); err != nil || len(g.Resources()) != 7 {
		t.Errorf("class dropped: %d", len(g.Resources()))
	}
	// A renamed pool: the old class stays in the store for the rename, but is no resource any more.
	svc.classes = []GPUClass{{ID: "maarm64-large", Pool: "maarm64", Tier: "large"}}
	_, _ = g.Refresh(ctx)
	svc.classes = []GPUClass{{ID: "ma-arm64-large", Pool: "ma-arm64", Tier: "large", Replaces: "maarm64-large"}}
	_, _ = g.Refresh(ctx)
	have := map[string]bool{}
	for _, r := range g.Resources() {
		have[r.ID] = true
	}
	if have["ws-gpus-maarm64-large"] || have["ws-gpu-hours-maarm64-large"] || !have["ws-gpus-ma-arm64-large"] {
		t.Errorf("renamed pool: %v", have)
	}
	if r := g.Renames(); r["ws-gpus-maarm64-large"] != "ws-gpus-ma-arm64-large" {
		t.Errorf("rename of the pool's class: %v", r)
	}
}
