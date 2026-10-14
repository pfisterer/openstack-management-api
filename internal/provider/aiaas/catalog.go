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
	// OnNew is told when the catalogue gained classes (the tree gives them to the root).
	OnNew func(ctx context.Context) error
}

func NewGPUCatalog(static []common.Resource, store ClassStore, service interface {
	Catalog(ctx context.Context) ([]GPUClass, error)
}, log *zap.SugaredLogger) *GPUCatalog {
	return &GPUCatalog{static: static, store: store, service: service, log: log}
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
	for _, c := range g.classes {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		for _, r := range []common.Resource{
			{ID: prefixGPUHours + c.ID, Name: "GPU hours " + name, Unit: "GPU h", Kind: common.KindWindow, Window: "30d", Group: "Workspaces GPU",
				Max: 20000, ShowOnUI: true, Provider: Provider, Message: "GPU hours per 30 days on GPUs of the class " + name},
			{ID: prefixGPUs + c.ID, Name: "GPUs " + name + " at once", Unit: "GPU", Group: "Workspaces GPU",
				Max: 64, ShowOnUI: true, Provider: Provider, Message: "how many GPUs of the class " + name + " the project may use at the same time"},
		} {
			if !have[r.ID] {
				out = append(out, r)
			}
		}
	}
	return out
}
