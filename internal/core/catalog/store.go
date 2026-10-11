package catalog

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ── Postgres ──────────────────────────────────────────────────────────────────

type dbEntry struct {
	ID          string    `gorm:"column:id;primaryKey"`
	Name        string    `gorm:"column:name;not null"`
	Group       string    `gorm:"column:resource_group"`
	Message     string    `gorm:"column:message"`
	GrantType   string    `gorm:"column:grant_type;not null"`
	GrantTarget string    `gorm:"column:grant_target;not null"`
	State       string    `gorm:"column:state;not null"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	CreatedBy   string    `gorm:"column:created_by"`
	ChangedAt   time.Time `gorm:"column:changed_at"`
	ChangedBy   string    `gorm:"column:changed_by"`
}

func (dbEntry) TableName() string { return "catalog_availabilities" }

// PostgresStore keeps the availabilities next to the tree, so the daily
// backups carry them too.
type PostgresStore struct{ db *gorm.DB }

// NewPostgresStore creates the table if needed.
func NewPostgresStore(db *gorm.DB) (*PostgresStore, error) {
	if err := db.AutoMigrate(&dbEntry{}); err != nil {
		return nil, fmt.Errorf("migrate catalog table: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) List(ctx context.Context) ([]Entry, error) {
	var rows []dbEntry
	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, Entry{ID: r.ID, Name: r.Name, Group: r.Group, Message: r.Message, State: r.State,
			CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, ChangedAt: r.ChangedAt, ChangedBy: r.ChangedBy,
			Grant: grant(r.GrantType, r.GrantTarget)})
	}
	return out, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, e Entry) error {
	row := dbEntry{ID: e.ID, Name: e.Name, Group: e.Group, Message: e.Message, GrantType: e.Grant.Type,
		GrantTarget: e.Grant.Target, State: e.State, CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy,
		ChangedAt: e.ChangedAt, ChangedBy: e.ChangedBy}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// ── Memory ────────────────────────────────────────────────────────────────────

// MemoryStore is for the in-memory deployment mode and tests.
type MemoryStore struct {
	mu      sync.Mutex
	entries []Entry
}

func (s *MemoryStore) List(context.Context) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.entries), nil
}

func (s *MemoryStore) Upsert(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.entries, func(x Entry) bool { return x.ID == e.ID }); i >= 0 {
		s.entries[i] = e
		return nil
	}
	s.entries = append(s.entries, e)
	return nil
}

func grant(typ, target string) common.Grant { return common.Grant{Type: typ, Target: target} }
