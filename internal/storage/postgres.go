package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ applogic.ProjectStore = (*PostgresProjectStore)(nil)

// ── GORM row models ────────────────────────────────────────────────────────────

type delegationRow struct {
	ID         string  `gorm:"primaryKey;column:id"`
	ParentID   *string `gorm:"column:parent_id;index"`
	AdminScope []byte  `gorm:"column:admin_scope;type:jsonb;not null"`
	DataJSON   []byte  `gorm:"column:data;type:jsonb;not null"`
}

func (delegationRow) TableName() string { return "delegations" }

type projectRow struct {
	ID              string  `gorm:"primaryKey;column:id"`
	Status          string  `gorm:"column:status;index"`
	FundedBy        *string `gorm:"column:funded_by;index"`
	RequesterTokens []byte  `gorm:"column:requester_tokens;type:jsonb;not null"`
	DataJSON        []byte  `gorm:"column:data;type:jsonb;not null"`
}

func (projectRow) TableName() string { return "projects" }

type eligibilityRuleRow struct {
	OwnerToken         string `gorm:"primaryKey;column:owner_token"`
	EligibleRequesters []byte `gorm:"column:eligible_requesters;type:jsonb;not null"`
	DataJSON           []byte `gorm:"column:data;type:jsonb;not null"`
}

func (eligibilityRuleRow) TableName() string { return "eligibility_rules" }

type identityRow struct {
	ID       string `gorm:"primaryKey;column:id"`
	DataJSON []byte `gorm:"column:data;type:jsonb;not null"`
}

func (identityRow) TableName() string { return "identities" }

// ── Store ──────────────────────────────────────────────────────────────────────

type PostgresProjectStore struct {
	db  *gorm.DB
	log *zap.SugaredLogger
}

func NewPostgresProjectStore(dsn string, log *zap.SugaredLogger) (*PostgresProjectStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := db.AutoMigrate(&delegationRow{}, &projectRow{}, &eligibilityRuleRow{}, &identityRow{}); err != nil {
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return &PostgresProjectStore{db: db, log: log}, nil
}

// ── Conversion helpers ─────────────────────────────────────────────────────────

func mustMarshalPG(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal: %v", err))
	}
	return b
}

func toDelegationRow(d common.Delegation) delegationRow {
	return delegationRow{
		ID:         d.ID,
		ParentID:   d.ParentID,
		AdminScope: mustMarshalPG(d.AdminScope),
		DataJSON:   mustMarshalPG(d),
	}
}

func fromDelegationRow(r delegationRow) (common.Delegation, error) {
	var d common.Delegation
	return d, json.Unmarshal(r.DataJSON, &d)
}

func fromDelegationRows(rows []delegationRow) ([]common.Delegation, error) {
	out := make([]common.Delegation, 0, len(rows))
	for _, r := range rows {
		d, err := fromDelegationRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func toProjectRow(p common.Project) projectRow {
	return projectRow{
		ID:              p.ID,
		Status:          p.Status,
		FundedBy:        p.FundedBy,
		RequesterTokens: mustMarshalPG(p.RequesterTokens),
		DataJSON:        mustMarshalPG(p),
	}
}

func fromProjectRow(r projectRow) (common.Project, error) {
	var p common.Project
	return p, json.Unmarshal(r.DataJSON, &p)
}

func fromProjectRows(rows []projectRow) ([]common.Project, error) {
	out := make([]common.Project, 0, len(rows))
	for _, r := range rows {
		p, err := fromProjectRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func toEligibilityRuleRow(rule common.TokenEligibilityRule) eligibilityRuleRow {
	return eligibilityRuleRow{
		OwnerToken:         rule.OwnerToken,
		EligibleRequesters: mustMarshalPG(rule.EligibleRequesters),
		DataJSON:           mustMarshalPG(rule),
	}
}

func fromEligibilityRuleRow(r eligibilityRuleRow) (common.TokenEligibilityRule, error) {
	var rule common.TokenEligibilityRule
	return rule, json.Unmarshal(r.DataJSON, &rule)
}

func toIdentityRow(ident common.Identity) identityRow {
	return identityRow{ID: ident.ID, DataJSON: mustMarshalPG(ident)}
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
	return db.Where(strings.Join(conds, " OR "), args...)
}

// ── State management ───────────────────────────────────────────────────────────

func (s *PostgresProjectStore) IsProjectStateEmpty(ctx context.Context) (bool, error) {
	models := []any{&identityRow{}, &delegationRow{}, &projectRow{}}
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

func (s *PostgresProjectStore) SeedProjectState(ctx context.Context, identities []common.Identity, delegations []common.Delegation, projects []common.Project, rules []common.TokenEligibilityRule) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("TRUNCATE TABLE identities, delegations, projects, eligibility_rules").Error; err != nil {
			return err
		}
		for _, ident := range identities {
			row := toIdentityRow(ident)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		for _, d := range delegations {
			row := toDelegationRow(d)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		for _, p := range projects {
			row := toProjectRow(p)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		for _, rule := range rules {
			row := toEligibilityRuleRow(rule)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PostgresProjectStore) ListIdentities(ctx context.Context) ([]common.Identity, error) {
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

// ListProjectParticipants loads every project and derives the distinct participant
// emails in Go. Participants live inside the JSONB `data` blob (requester +
// authorized-user tokens), so there is no cheap column to DISTINCT on; at the
// current scale a full scan is fine. This is the natural spot to later back with a
// materialized, searchable principals table (see __TODOS os2).
func (s *PostgresProjectStore) ListProjectParticipants(ctx context.Context) ([]string, error) {
	var rows []projectRow
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	projects, err := fromProjectRows(rows)
	if err != nil {
		return nil, err
	}
	return common.ParticipantEmails(projects), nil
}

// ── Delegation operations ──────────────────────────────────────────────────────

func (s *PostgresProjectStore) GetDelegationByID(ctx context.Context, id string) (*common.Delegation, error) {
	var row delegationRow
	err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := fromDelegationRow(row)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PostgresProjectStore) ListDelegationsByParentIDs(ctx context.Context, parentIDs []string, limit, offset int) ([]common.Delegation, error) {
	if len(parentIDs) == 0 {
		return []common.Delegation{}, nil
	}
	var rows []delegationRow
	err := s.db.WithContext(ctx).
		Where("parent_id IN ?", parentIDs).
		Order("id ASC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return fromDelegationRows(rows)
}

func (s *PostgresProjectStore) GetDelegationsByAdminScope(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if len(userTokens) == 0 {
		return []common.Delegation{}, nil
	}
	var rows []delegationRow
	err := jsonbContainsAny(s.db.WithContext(ctx), "admin_scope", userTokens).
		Order("id ASC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	s.log.Debugw("GetDelegationsByAdminScope", "userTokens", userTokens, "returned", len(rows))
	return fromDelegationRows(rows)
}

func (s *PostgresProjectStore) GetDelegationsByParentTokens(ctx context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if len(userTokens) == 0 {
		return []common.Delegation{}, nil
	}
	var rows []delegationRow
	err := s.db.WithContext(ctx).
		Where("parent_id IN ?", []string(userTokens)).
		Order("id ASC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	s.log.Debugw("GetDelegationsByParentTokens", "userTokens", userTokens, "returned", len(rows))
	return fromDelegationRows(rows)
}

func (s *PostgresProjectStore) UpsertDelegation(ctx context.Context, delegation common.Delegation) error {
	row := toDelegationRow(delegation)
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *PostgresProjectStore) DeleteDelegations(ctx context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Where("id IN ?", delegationIDs).Delete(&delegationRow{}).Error
}

// ── Project operations ─────────────────────────────────────────────────────────

func (s *PostgresProjectStore) GetProjectByID(ctx context.Context, id string) (*common.Project, error) {
	var row projectRow
	err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := fromProjectRow(row)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PostgresProjectStore) ListProjectsBy(ctx context.Context, userEmail string, limit, offset int) ([]common.Project, error) {
	userToken := "user:" + userEmail
	var rows []projectRow
	err := s.db.WithContext(ctx).
		Where("requester_tokens @> ?::jsonb", string(mustMarshalPG([]string{userToken}))).
		Order("id DESC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	projects, err := fromProjectRows(rows)
	s.log.Debugw("ListProjectsBy", "userEmail", userEmail, "returned", len(projects))
	return projects, err
}

func (s *PostgresProjectStore) GetProjectsByFundedByIDs(ctx context.Context, delegationIDs []string, statuses []string, limit, offset int) ([]common.Project, error) {
	if len(delegationIDs) == 0 || len(statuses) == 0 {
		return []common.Project{}, nil
	}
	var rows []projectRow
	err := s.db.WithContext(ctx).
		Where("funded_by IN ?", delegationIDs).
		Where("status IN ?", statuses).
		Order("id DESC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return fromProjectRows(rows)
}

func (s *PostgresProjectStore) UpsertProject(ctx context.Context, project common.Project) error {
	row := toProjectRow(project)
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *PostgresProjectStore) ListProjectsByStatus(ctx context.Context, statuses []string, limit, offset int) ([]common.Project, error) {
	if len(statuses) == 0 {
		return []common.Project{}, nil
	}
	var rows []projectRow
	err := s.db.WithContext(ctx).
		Where("status IN ?", statuses).
		Order("id DESC").
		Limit(normalizeLimit(limit)).Offset(normalizeOffset(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return fromProjectRows(rows)
}

func (s *PostgresProjectStore) DeleteProject(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Where("id = ?", id).Delete(&projectRow{}).Error
}

func (s *PostgresProjectStore) ClearProjectFundingByDelegationIDs(ctx context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	var rows []projectRow
	if err := s.db.WithContext(ctx).Where("funded_by IN ?", delegationIDs).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		p, err := fromProjectRow(row)
		if err != nil {
			return err
		}
		p.FundedBy = nil
		if err := s.UpsertProject(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// ── Eligibility rule operations ────────────────────────────────────────────────

func (s *PostgresProjectStore) GetEligibilityRulesByOwnerTokens(ctx context.Context, ownerTokens []string) ([]common.TokenEligibilityRule, error) {
	if len(ownerTokens) == 0 {
		return []common.TokenEligibilityRule{}, nil
	}
	var rows []eligibilityRuleRow
	if err := s.db.WithContext(ctx).Where("owner_token IN ?", ownerTokens).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]common.TokenEligibilityRule, 0, len(rows))
	for _, r := range rows {
		rule, err := fromEligibilityRuleRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

func (s *PostgresProjectStore) GetEligibilityRulesByRequesterTokens(ctx context.Context, requesterTokens []string) ([]common.TokenEligibilityRule, error) {
	if len(requesterTokens) == 0 {
		return []common.TokenEligibilityRule{}, nil
	}
	var rows []eligibilityRuleRow
	if err := jsonbContainsAny(s.db.WithContext(ctx), "eligible_requesters", requesterTokens).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]common.TokenEligibilityRule, 0, len(rows))
	for _, r := range rows {
		rule, err := fromEligibilityRuleRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

func (s *PostgresProjectStore) UpsertEligibilityRule(ctx context.Context, rule common.TokenEligibilityRule) error {
	row := toEligibilityRuleRow(rule)
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *PostgresProjectStore) DeleteEligibilityRule(ctx context.Context, ownerToken string) error {
	return s.db.WithContext(ctx).Where("owner_token = ?", ownerToken).Delete(&eligibilityRuleRow{}).Error
}
