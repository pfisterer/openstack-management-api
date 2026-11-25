package nodestore

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// migrateAllocationLeaves runs MigrateToAllocations on the stored nodes in one transaction, at startup before anything reads them: the table is locked, the new nodes are written, read back and checked (same projects, the sum of every project's allocations what it had before), and anything off rolls the whole migration back and fails the start. A store migrated already is left alone. dryRun does everything but commit.
func migrateAllocationLeaves(db *gorm.DB, log *zap.SugaredLogger, dryRun bool) (AllocationMigrationReport, error) {
	var rep AllocationMigrationReport
	errDryRun := fmt.Errorf("dry run")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("LOCK TABLE nodes IN EXCLUSIVE MODE").Error; err != nil {
			return fmt.Errorf("lock nodes: %w", err)
		}
		var waiting int64
		if err := tx.Model(&nodeRow{}).Where("kind = ? AND parent_id IS NOT NULL", model.KindProject).Count(&waiting).Error; err != nil {
			return err
		}
		if waiting == 0 {
			return nil
		}
		var rows []nodeRow
		if err := tx.Where("kind = ?", model.KindProject).Find(&rows).Error; err != nil {
			return err
		}
		legacy := make([]LegacyNode, 0, len(rows))
		for _, r := range rows {
			n, err := fromNodeRow(r)
			if err != nil {
				return fmt.Errorf("read %s: %w", r.ID, err)
			}
			var extras struct {
				Allocations []LegacyAllocation `json:"allocations"`
			}
			if err := json.Unmarshal(r.DataJSON, &extras); err != nil {
				return fmt.Errorf("read allocations of %s: %w", r.ID, err)
			}
			legacy = append(legacy, LegacyNode{Node: n, Extras: extras.Allocations})
		}
		writes, r, err := MigrateToAllocations(legacy, time.Now())
		rep = r
		if err != nil {
			return err
		}
		if len(rep.Mismatches) > 0 {
			return fmt.Errorf("%d projects would not keep their limit, e.g. %s", len(rep.Mismatches), rep.Mismatches[0])
		}
		for _, n := range writes {
			row := toNodeRow(n)
			if err := tx.Save(&row).Error; err != nil {
				return fmt.Errorf("write %s: %w", n.ID, err)
			}
		}
		if err := verifyAllocationMigration(tx, legacy, &rep); err != nil {
			return err
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err == errDryRun {
		err = nil
	}
	if err == nil && rep.Projects > 0 && log != nil {
		log.Infow("Projects migrated to allocation leaves", "dry_run", dryRun, "projects", rep.Projects, "allocations", rep.Allocations, "extra_allocations", rep.ExtraAllocations, "pending_changes", rep.PendingChanges, "imported", rep.Imported, "dropped_member_changes", rep.DroppedMemberChange)
	}
	return rep, err
}

// verifyAllocationMigration reads the migrated nodes back inside the transaction and checks what the acceptance asks for: as many projects as before, none of them in the tree any more, and every project's allocations adding up to what it held before.
func verifyAllocationMigration(tx *gorm.DB, before []LegacyNode, rep *AllocationMigrationReport) error {
	var projects []nodeRow
	if err := tx.Where("kind = ?", model.KindProject).Find(&projects).Error; err != nil {
		return err
	}
	rep.ProjectsAfter = len(projects)
	if len(projects) != len(before) {
		return fmt.Errorf("%d projects after the migration, %d before", len(projects), len(before))
	}
	var allocRows []nodeRow
	if err := tx.Where("kind = ?", model.KindAllocation).Find(&allocRows).Error; err != nil {
		return err
	}
	allocs, err := fromNodeRows(allocRows)
	if err != nil {
		return err
	}
	byProject := map[string][]model.Node{}
	for _, a := range allocs {
		byProject[a.ProjectID] = append(byProject[a.ProjectID], a)
	}
	for _, l := range before {
		p := l.Node
		for _, row := range projects {
			if row.ID == p.ID && row.ParentID != nil {
				return fmt.Errorf("project %s is still in the tree", p.ID)
			}
		}
		if p.Status == model.StatusImported {
			continue
		}
		want := SumAllocations([]model.Node{{Limit: p.Limit}}, nil)
		for _, a := range l.Extras {
			want = SumAllocations([]model.Node{{Limit: want}, {Limit: a.Limit}}, nil)
		}
		if got := SumAllocations(byProject[p.ID], nil); !QuotaEqual(got, want) {
			return fmt.Errorf("project %s: allocations add up to %v, it held %v", p.ID, got, want)
		}
	}
	return nil
}

// DryRunAllocationMigration connects to the database and runs the migration to allocation leaves without committing anything, for a trial run against a copy of a database (MIGRATE_DRY_RUN). Nothing else is migrated or written.
func DryRunAllocationMigration(dsn string, log *zap.SugaredLogger) (AllocationMigrationReport, error) {
	db, err := openPostgres(dsn, log)
	if err != nil {
		return AllocationMigrationReport{}, err
	}
	if err := db.AutoMigrate(&nodeRow{}); err != nil {
		return AllocationMigrationReport{}, fmt.Errorf("postgres: migrate: %w", err)
	}
	return migrateAllocationLeaves(db, log, true)
}
