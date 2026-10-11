package tree

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// legacyTables are the tables of the model this package replaced.
//
// The tree rewrite moved everything into `nodes` and `identities` and stopped
// referencing these three. Nothing removed them, because nothing does: GORM's
// AutoMigrate adds tables and columns and widens them, but never drops. So they
// have sat in both databases ever since, describing a model no code can read any
// more — which is exactly the kind of thing that is later mistaken for data.
//
// Contents when this was written (2026-08-26): on production all three were
// empty; on staging `delegations` held two rows from 2026-07-10, "Organization
// Root" and "WI-Labor", superseded by the `nodes` root created on 2026-07-30.
// They were backed up before this migration first ran.
var legacyTables = []string{"delegations", "projects", "eligibility_rules"}

// dropLegacyTables removes them, once, at startup.
//
// Deliberately explicit rather than clever: a table is dropped only if it is in
// the list above AND actually present, so this is a no-op on every database that
// has already been through it, and it can never touch a table the model still
// uses — `nodes`, `identities` and `tokens` are not in the list, and adding one
// of them would have to be a deliberate act, visible in a diff.
func dropLegacyTables(db *gorm.DB, log *zap.SugaredLogger) error {
	m := db.Migrator()

	for _, table := range legacyTables {
		if !m.HasTable(table) {
			continue
		}
		// Say how much is being thrown away before throwing it away. On an empty
		// table this is a zero nobody needs to read; on a non-empty one it is the
		// only record that anything was there at all.
		var rows int64
		if err := db.Table(table).Count(&rows).Error; err != nil {
			return fmt.Errorf("dropLegacyTables: counting %s: %w", table, err)
		}
		if log != nil {
			log.Infow("dropping a table left over from the model before the tree rewrite",
				"table", table, "rows", rows)
		}
		if err := m.DropTable(table); err != nil {
			return fmt.Errorf("dropLegacyTables: dropping %s: %w", table, err)
		}
	}
	return nil
}
