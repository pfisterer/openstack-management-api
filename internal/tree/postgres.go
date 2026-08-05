package tree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ Store = (*PostgresStore)(nil)

// ── GORM row models ────────────────────────────────────────────────────────────
// One table: a thin indexed shell around a JSONB `data` blob holding the full
// node. The extra columns exist only for querying.

type nodeRow struct {
	ID                 string  `gorm:"primaryKey;column:id"`
	ParentID           *string `gorm:"column:parent_id;index"`
	Kind               string  `gorm:"column:kind;index"`
	Status             string  `gorm:"column:status;index"`
	Owner              string  `gorm:"column:owner;index"`
	AdminScope         []byte  `gorm:"column:admin_scope;type:jsonb;not null"`
	EligibleRequesters []byte  `gorm:"column:eligible_requesters;type:jsonb;not null"`
	DataJSON           []byte  `gorm:"column:data;type:jsonb;not null"`
}

func (nodeRow) TableName() string { return "nodes" }

type identityRow struct {
	ID       string `gorm:"primaryKey;column:id"`
	DataJSON []byte `gorm:"column:data;type:jsonb;not null"`
}

func (identityRow) TableName() string { return "identities" }

// ── Store ──────────────────────────────────────────────────────────────────────

type PostgresStore struct {
	db  *gorm.DB
	log *zap.SugaredLogger
}

func NewPostgresStore(dsn string, log *zap.SugaredLogger) (*PostgresStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := db.AutoMigrate(&nodeRow{}, &identityRow{}); err != nil {
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return &PostgresStore{db: db, log: log}, nil
}

// ── Conversion helpers ─────────────────────────────────────────────────────────

func mustMarshalPG(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal: %v", err))
	}
	return b
}

func toNodeRow(n Node) nodeRow {
	// Usage is a response-only computed field — never persist it.
	n.Usage = nil
	return nodeRow{
		ID:                 n.ID,
		ParentID:           n.ParentID,
		Kind:               n.Kind,
		Status:             n.Status,
		Owner:              n.Owner,
		AdminScope:         mustMarshalPG(n.AdminScope),
		EligibleRequesters: mustMarshalPG(n.EligibleRequesters),
		DataJSON:           mustMarshalPG(n),
	}
}

func fromNodeRow(r nodeRow) (Node, error) {
	var n Node
	return n, json.Unmarshal(r.DataJSON, &n)
}

func fromNodeRows(rows []nodeRow) ([]Node, error) {
	out := make([]Node, 0, len(rows))
	for _, r := range rows {
		n, err := fromNodeRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// normalizeLimit maps limit <= 0 to -1 (GORM's "no LIMIT" sentinel).
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return -1
	}
	return limit
}

func normalizeOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

// jsonbContainsAny appends OR conditions that check whether a jsonb array column
// contains any of the given string values using the @> containment operator.
// The ORs are parenthesized explicitly so the clause composes correctly with
// other AND-joined filters in the same query.
func jsonbContainsAny(db *gorm.DB, column string, tokens []string) *gorm.DB {
	if len(tokens) == 0 {
		return db.Where("FALSE")
	}
	conds := make([]string, len(tokens))
	args := make([]any, len(tokens))
	for i, token := range tokens {
		conds[i] = column + " @> ?::jsonb"
		args[i] = string(mustMarshalPG([]string{token}))
	}
	return db.Where("("+strings.Join(conds, " OR ")+")", args...)
}

// ── Store implementation ───────────────────────────────────────────────────────

func (s *PostgresStore) IsEmpty(ctx context.Context) (bool, error) {
	models := []any{&identityRow{}, &nodeRow{}}
	for _, model := range models {
		var count int64
		if err := s.db.WithContext(ctx).Model(model).Count(&count).Error; err != nil {
			return false, err
		}
		if count > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (s *PostgresStore) Seed(ctx context.Context, identities []common.Identity, nodes []Node) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("TRUNCATE TABLE identities, nodes").Error; err != nil {
			return err
		}
		for _, ident := range identities {
			row := identityRow{ID: ident.ID, DataJSON: mustMarshalPG(ident)}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		for _, n := range nodes {
			row := toNodeRow(n)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PostgresStore) ListIdentities(ctx context.Context) ([]common.Identity, error) {
	var rows []identityRow
	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]common.Identity, 0, len(rows))
	for _, r := range rows {
		var ident common.Identity
		if err := json.Unmarshal(r.DataJSON, &ident); err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, nil
}

// ListParticipants loads the leaves and derives distinct participant emails in
// Go. Participants live inside the JSONB `data` blob (owner + authorized-user
// tokens); at the current scale a full scan is fine.
func (s *PostgresStore) ListParticipants(ctx context.Context) ([]string, error) {
	var rows []nodeRow
	if err := s.db.WithContext(ctx).Where("kind = ?", KindProject).Find(&rows).Error; err != nil {
		return nil, err
	}
	nodes, err := fromNodeRows(rows)
	if err != nil {
		return nil, err
	}
	return ParticipantEmails(nodes), nil
}

func (s *PostgresStore) GetNode(ctx context.Context, id string) (*Node, error) {
	var row nodeRow
	err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n, err := fromNodeRow(row)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *PostgresStore) ListNodes(ctx context.Context, q NodeQuery, limit, offset int) ([]Node, error) {
	db := s.db.WithContext(ctx)
	if len(q.IDs) > 0 {
		db = db.Where("id IN ?", q.IDs)
	}
	if len(q.ParentIDs) > 0 {
		db = db.Where("parent_id IN ?", q.ParentIDs)
	}
	if len(q.Kinds) > 0 {
		db = db.Where("kind IN ?", q.Kinds)
	}
	if len(q.Statuses) > 0 {
		db = db.Where("status IN ?", q.Statuses)
	}
	if q.Owner != "" {
		db = db.Where("owner = ?", q.Owner)
	}
	if len(q.AdminScopeAny) > 0 {
		db = jsonbContainsAny(db, "admin_scope", q.AdminScopeAny)
	}
	if len(q.EligibleAny) > 0 {
		db = jsonbContainsAny(db, "eligible_requesters", q.EligibleAny)
	}

	var rows []nodeRow
	err := db.Order("id ASC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return fromNodeRows(rows)
}

func (s *PostgresStore) UpsertNode(ctx context.Context, n Node) error {
	row := toNodeRow(n)
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// CountChildren counts direct children per parent in one grouped query.
func (s *PostgresStore) CountChildren(ctx context.Context, parentIDs []string) (map[string]int, error) {
	if len(parentIDs) == 0 {
		return map[string]int{}, nil
	}
	var rows []struct {
		ParentID string
		N        int
	}
	if err := s.db.WithContext(ctx).
		Model(&nodeRow{}).
		Select("parent_id, count(*) as n").
		Where("parent_id IN ?", parentIDs).
		Group("parent_id").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("count children: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.ParentID] = r.N
	}
	return counts, nil
}

func (s *PostgresStore) DeleteNodes(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&nodeRow{}).Error
}
