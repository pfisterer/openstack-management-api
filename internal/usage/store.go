package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/tree"
	"gorm.io/gorm"
)

// ── Postgres ──────────────────────────────────────────────────────────────────

type dbDay struct {
	Day         time.Time `gorm:"column:day;type:date;primaryKey"`
	NodeID      string    `gorm:"column:node_id;primaryKey"`
	OSProjectID string    `gorm:"column:os_project_id;index"`
	ProjectName string    `gorm:"column:project_name"`
	Owner       string    `gorm:"column:owner;index"`
	Status      string    `gorm:"column:status"`
	BudgetID    string    `gorm:"column:budget_id;index"`
	BudgetPath  []byte    `gorm:"column:budget_path;type:jsonb;not null"`
	People      int       `gorm:"column:people"`
	Groups      int       `gorm:"column:member_groups"`
	ServerHours float64   `gorm:"column:server_hours"`
	VCPUHours   float64   `gorm:"column:vcpu_hours"`
	RAMGBHours  float64   `gorm:"column:ram_gb_hours"`
	DiskGBHours float64   `gorm:"column:disk_gb_hours"`
	StorageGB   float64   `gorm:"column:storage_gb"`
	PublicIPv4  *int      `gorm:"column:public_ipv4"`
	Reserved    []byte    `gorm:"column:reserved;type:jsonb;not null"`
	// Nullable: the rows from before attributes existed have none.
	Attributes  []byte    `gorm:"column:attributes;type:jsonb"`
	Backfilled  bool      `gorm:"column:backfilled"`
	CollectedAt time.Time `gorm:"column:collected_at"`
}

func (dbDay) TableName() string { return "project_usage_daily" }

// PostgresStore keeps the rows in the service's own database, next to the tree,
// so the daily backups carry them too.
type PostgresStore struct{ db *gorm.DB }

// NewPostgresStore creates the table if needed.
func NewPostgresStore(db *gorm.DB) (*PostgresStore, error) {
	if err := db.AutoMigrate(&dbDay{}); err != nil {
		return nil, fmt.Errorf("migrate usage table: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) ReplaceDay(ctx context.Context, day time.Time, rows []Day) error {
	day = dayOf(day)
	recs := make([]dbDay, 0, len(rows))
	for _, r := range rows {
		path, err := json.Marshal(r.BudgetPath)
		if err != nil {
			return err
		}
		reserved, err := json.Marshal(r.Reserved)
		if err != nil {
			return err
		}
		var attrs []byte
		if len(r.Attributes) > 0 {
			if attrs, err = json.Marshal(r.Attributes); err != nil {
				return err
			}
		}
		recs = append(recs, dbDay{
			Day: day, NodeID: r.NodeID, OSProjectID: r.OSProjectID, ProjectName: r.ProjectName,
			Owner: r.Owner, Status: r.Status, BudgetID: r.BudgetID, BudgetPath: path,
			People: r.People, Groups: r.Groups,
			ServerHours: r.ServerHours, VCPUHours: r.VCPUHours, RAMGBHours: r.RAMGBHours,
			DiskGBHours: r.DiskGBHours, StorageGB: r.StorageGB, PublicIPv4: r.PublicIPv4, Reserved: reserved,
			Attributes: attrs, Backfilled: r.Backfilled, CollectedAt: r.CollectedAt,
		})
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("day = ?", day).Delete(&dbDay{}).Error; err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		return tx.CreateInBatches(recs, 200).Error
	})
}

func (s *PostgresStore) LastDay(ctx context.Context) (time.Time, bool, error) {
	// NullTime, not *time.Time: MAX over an empty table is NULL, and the
	// driver will not scan that into a pointer.
	var last sql.NullTime
	if err := s.db.WithContext(ctx).Model(&dbDay{}).Select("MAX(day)").Row().Scan(&last); err != nil {
		return time.Time{}, false, err
	}
	if !last.Valid {
		return time.Time{}, false, nil
	}
	return dayOf(last.Time), true, nil
}

func (s *PostgresStore) Days(ctx context.Context, nodeIDs []string, from, to time.Time) ([]Day, error) {
	q := s.db.WithContext(ctx).Where("day >= ? AND day < ?", dayOf(from), dayOf(to))
	if len(nodeIDs) > 0 {
		q = q.Where("node_id IN ?", nodeIDs)
	}
	return s.find(q)
}

func (s *PostgresStore) DaysUnder(ctx context.Context, budgetID string, from, to time.Time) ([]Day, error) {
	// jsonb containment: the path holds an entry with this id.
	needle, err := json.Marshal([]PathEntry{{ID: budgetID}})
	if err != nil {
		return nil, err
	}
	q := s.db.WithContext(ctx).Where("day >= ? AND day < ?", dayOf(from), dayOf(to)).
		Where("budget_path @> ?::jsonb", string(needle))
	return s.find(q)
}

func (s *PostgresStore) find(q *gorm.DB) ([]Day, error) {
	var recs []dbDay
	if err := q.Order("day, node_id").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]Day, 0, len(recs))
	for _, r := range recs {
		d := Day{
			Day: dayOf(r.Day), NodeID: r.NodeID, OSProjectID: r.OSProjectID, ProjectName: r.ProjectName,
			Owner: r.Owner, Status: r.Status, BudgetID: r.BudgetID, People: r.People, Groups: r.Groups,
			ServerHours: r.ServerHours, VCPUHours: r.VCPUHours, RAMGBHours: r.RAMGBHours,
			DiskGBHours: r.DiskGBHours, StorageGB: r.StorageGB, PublicIPv4: r.PublicIPv4,
			Backfilled: r.Backfilled, CollectedAt: r.CollectedAt,
		}
		_ = json.Unmarshal(r.BudgetPath, &d.BudgetPath)
		_ = json.Unmarshal(r.Reserved, &d.Reserved)
		if len(r.Attributes) > 0 {
			_ = json.Unmarshal(r.Attributes, &d.Attributes)
		}
		out = append(out, d)
	}
	return out, nil
}

// ── Memory ────────────────────────────────────────────────────────────────────

// MemoryStore is for development and tests; it forgets everything on restart.
type MemoryStore struct {
	mu   sync.Mutex
	days map[time.Time][]Day
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{days: map[time.Time][]Day{}} }

func (s *MemoryStore) ReplaceDay(_ context.Context, day time.Time, rows []Day) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day = dayOf(day)
	out := make([]Day, len(rows))
	for i, r := range rows {
		r.Day = day
		r.Reserved = maps.Clone(r.Reserved)
		r.BudgetPath = slices.Clone(r.BudgetPath)
		r.Attributes = cloneAttributes(r.Attributes)
		out[i] = r
	}
	s.days[day] = out
	return nil
}

func (s *MemoryStore) LastDay(_ context.Context) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last time.Time
	ok := false
	for d := range s.days {
		if !ok || d.After(last) {
			last, ok = d, true
		}
	}
	return last, ok, nil
}

func (s *MemoryStore) Days(_ context.Context, nodeIDs []string, from, to time.Time) ([]Day, error) {
	return s.filter(from, to, func(r Day) bool { return len(nodeIDs) == 0 || slices.Contains(nodeIDs, r.NodeID) }), nil
}

func (s *MemoryStore) DaysUnder(_ context.Context, budgetID string, from, to time.Time) ([]Day, error) {
	return s.filter(from, to, func(r Day) bool {
		return slices.ContainsFunc(r.BudgetPath, func(p PathEntry) bool { return p.ID == budgetID })
	}), nil
}

func (s *MemoryStore) filter(from, to time.Time, keep func(Day) bool) []Day {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to = dayOf(from), dayOf(to)
	var out []Day
	for d, rows := range s.days {
		if d.Before(from) || !d.Before(to) {
			continue
		}
		for _, r := range rows {
			if keep(r) {
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b Day) int {
		if c := a.Day.Compare(b.Day); c != 0 {
			return c
		}
		if a.NodeID < b.NodeID {
			return -1
		}
		if a.NodeID > b.NodeID {
			return 1
		}
		return 0
	})
	return out
}

func cloneAttributes(a tree.Attributes) tree.Attributes {
	if a == nil {
		return nil
	}
	out := make(tree.Attributes, len(a))
	for g, v := range a {
		out[g] = maps.Clone(v)
	}
	return out
}
