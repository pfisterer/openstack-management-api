package aiaas

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// GPU classes from the AI service (its GPU administration: a class is a pool and a tier, docs/bauplan.md 10.13 of aiaas-deployment). Each class becomes two resources of the tree: ws-gpus-<class> (GPUs at the same time) and ws-gpu-hours-<class> (GPU hours per 30 days). A new class appears in the tree without a configuration change; the root gets it unlimited and root admins grant it down like any quantity — to the budgets of whoever the pool is for. Classes are never removed here: projects hold their resources.

// GPUClass is a class as the AI service's catalogue names it.
type GPUClass struct {
	ID   string `json:"id" gorm:"column:id;primaryKey"`
	Name string `json:"name" gorm:"column:name"`
	Pool string `json:"pool" gorm:"column:pool"`
	Tier string `json:"tier" gorm:"column:tier"`
	// Replaces is the id of a class from before pools this one took over: the tree's resources of that id are renamed to this one's.
	Replaces string `json:"replaces,omitempty" gorm:"column:replaces"`
	// For the UI: the names of pool and tier, the GPU types of its nodes and whether its pool lends idle GPUs to the other pools of the tier.
	PoolName string `json:"pool_name" gorm:"column:pool_name"`
	TierName string `json:"tier_name" gorm:"column:tier_name"`
	Products string `json:"products" gorm:"column:products"`
	Lends    bool   `json:"lends" gorm:"column:lends"`
}

func (GPUClass) TableName() string { return "aiaas_gpu_classes" }

// Catalog fetches the AI service's catalogue (GET /v1/tree/catalog), of which the tree needs the GPU classes.
func (c *Client) Catalog(ctx context.Context) ([]GPUClass, error) {
	var out struct {
		GPUClasses []GPUClass `json:"gpuClasses"`
	}
	return out.GPUClasses, c.do(ctx, http.MethodGet, "/v1/tree/catalog", nil, &out)
}

// ClassStore keeps the classes seen, so the tree knows them at the next start even when the AI service does not answer then.
type ClassStore interface {
	Load(ctx context.Context) ([]GPUClass, error)
	Save(ctx context.Context, classes []GPUClass) error
}

// MemoryClassStore is the ClassStore of the memory mode.
type MemoryClassStore struct {
	mu      sync.Mutex
	classes []GPUClass
}

func (m *MemoryClassStore) Load(context.Context) ([]GPUClass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.classes), nil
}

func (m *MemoryClassStore) Save(_ context.Context, classes []GPUClass) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.classes = slices.Clone(classes)
	return nil
}

// PostgresClassStore keeps them next to the tree.
type PostgresClassStore struct{ db *gorm.DB }

func NewPostgresClassStore(db *gorm.DB) (*PostgresClassStore, error) {
	if err := db.AutoMigrate(&GPUClass{}); err != nil {
		return nil, fmt.Errorf("migrate aiaas_gpu_classes: %w", err)
	}
	return &PostgresClassStore{db: db}, nil
}

func (p *PostgresClassStore) Load(ctx context.Context) ([]GPUClass, error) {
	var out []GPUClass
	return out, p.db.WithContext(ctx).Order("id").Find(&out).Error
}

// Save adds and renames; it never deletes.
func (p *PostgresClassStore) Save(ctx context.Context, classes []GPUClass) error {
	if len(classes) == 0 {
		return nil
	}
	return p.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&classes).Error
}

// GPUCatalog is the configured resources (PROVIDER_RESOURCES) plus the two resources of every GPU class the AI service has. A class whose ids the configuration already has keeps the configured ones.
type GPUCatalog struct {
	static  []common.Resource
	store   ClassStore
	service interface {
		Catalog(ctx context.Context) ([]GPUClass, error)
	}
	log *zap.SugaredLogger

	mu      sync.RWMutex
	classes []GPUClass
	renamed map[string]bool
	// OnNew is told when the catalogue gained classes (the tree gives them to the root).
	OnNew func(ctx context.Context) error
	// OnRename is told the resource ids of replaced classes, old to new (the tree rewrites its nodes); idempotent, so it runs after every refresh with renames.
	OnRename func(ctx context.Context, renames map[string]string) error
}

// pendingRenames are the renames not yet carried out since the start: once per process is enough, the tree keeps no old id afterwards.
func (g *GPUCatalog) pendingRenames() map[string]string {
	out := g.Renames()
	g.mu.RLock()
	defer g.mu.RUnlock()
	for id := range out {
		if g.renamed[id] {
			delete(out, id)
		}
	}
	return out
}

// Renames are the resource ids of replaced classes, old to new.
func (g *GPUCatalog) Renames() map[string]string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := map[string]string{}
	for _, c := range g.classes {
		if c.Replaces != "" && c.Replaces != c.ID {
			out[prefixGPUs+c.Replaces] = prefixGPUs + c.ID
			out[prefixGPUHours+c.Replaces] = prefixGPUHours + c.ID
		}
	}
	return out
}

func NewGPUCatalog(static []common.Resource, store ClassStore, service interface {
	Catalog(ctx context.Context) ([]GPUClass, error)
}, log *zap.SugaredLogger) *GPUCatalog {
	return &GPUCatalog{static: static, store: store, service: service, log: log, renamed: map[string]bool{}}
}

// Load reads the classes seen before; run once before the tree starts.
func (g *GPUCatalog) Load(ctx context.Context) error {
	classes, err := g.store.Load(ctx)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.classes = classes
	g.mu.Unlock()
	return nil
}

// Refresh asks the AI service and keeps what it has gained or renamed; true when the tree learned new classes.
func (g *GPUCatalog) Refresh(ctx context.Context) (bool, error) {
	fresh, err := g.service.Catalog(ctx)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	byID := map[string]GPUClass{}
	for _, c := range g.classes {
		byID[c.ID] = c
	}
	added, changed := false, false
	for _, c := range fresh {
		if c.ID == "" {
			continue
		}
		cur, ok := byID[c.ID]
		if !ok {
			added = true
		}
		if !ok || cur != c {
			changed = true
			byID[c.ID] = c
		}
	}
	if changed {
		g.classes = make([]GPUClass, 0, len(byID))
		for _, c := range byID {
			g.classes = append(g.classes, c)
		}
		sort.Slice(g.classes, func(i, j int) bool { return g.classes[i].ID < g.classes[j].ID })
	}
	all := slices.Clone(g.classes)
	g.mu.Unlock()
	if renames := g.pendingRenames(); len(renames) > 0 && g.OnRename != nil {
		if added && g.OnNew != nil {
			// The new ids first, so the root holds them when the old ones move over.
			if err := g.OnNew(ctx); err != nil {
				return added, err
			}
		}
		if err := g.OnRename(ctx, renames); err != nil {
			return added, err
		}
		g.mu.Lock()
		for id := range renames {
			g.renamed[id] = true
		}
		g.mu.Unlock()
	}
	if !changed {
		return false, nil
	}
	if err := g.store.Save(ctx, all); err != nil {
		return added, err
	}
	if added && g.OnNew != nil {
		return true, g.OnNew(ctx)
	}
	return added, nil
}

// Run refreshes every interval until ctx ends.
func (g *GPUCatalog) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if added, err := g.Refresh(ctx); err != nil {
			g.log.Warnw("aiaas: GPU classes not refreshed", "error", err)
		} else if added {
			g.log.Infow("aiaas: new GPU classes in the tree")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Resources is the configured resources and the GPU classes' ones.
func (g *GPUCatalog) Resources() []common.Resource {
	out := slices.Clone(g.static)
	have := map[string]bool{}
	for _, r := range out {
		have[r.ID] = true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	// A class another one replaces (its pool or tier renamed) stays in the store for the rename, but is no resource any more: the tree moved its amounts to the new id.
	replaced := map[string]bool{}
	for _, c := range g.classes {
		if c.Replaces != "" && c.Replaces != c.ID {
			replaced[c.Replaces] = true
		}
	}
	for _, c := range g.classes {
		if replaced[c.ID] {
			continue
		}
		name := c.Name
		if name == "" {
			name = c.ID
		}
		info := &common.GPUClassInfo{Pool: c.Pool, PoolName: c.PoolName, Tier: c.Tier, TierName: c.TierName, Products: c.Products, Lends: c.Lends}
		for _, r := range []common.Resource{
			{ID: prefixGPUHours + c.ID, Name: name + ": GPU hours", Unit: "GPU h", Kind: common.KindWindow, Window: "30d", Group: "Workspaces GPU",
				Max: 20000, ShowOnUI: true, Provider: Provider, Message: "GPU hours per 30 days on GPUs of the class " + name, GPUClass: info, Area: common.AreaWorkspaces},
			{ID: prefixGPUs + c.ID, Name: name + ": GPUs at the same time", Unit: "GPU", Group: "Workspaces GPU",
				Max: 64, ShowOnUI: true, Provider: Provider, Message: "how many GPUs of the class " + name + " the project may use at the same time", GPUClass: info, Area: common.AreaWorkspaces},
		} {
			if !have[r.ID] {
				out = append(out, r)
			}
		}
	}
	return out
}
