package tree

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// renamedNodeFields are keys of the stored node document that were renamed
// when the tree stopped naming OpenStack: what the executing system reports is
// no longer "os_…". The node is one JSON document, so AutoMigrate cannot see
// the change; without this a stored leaf would lose its project ID and its
// measurements on the first read.
var renamedNodeFields = [][2]string{
	{"os_project_id", "external_id"},
	{"os_project_name", "external_name"},
	{"os_overcommitted", "overcommitted"},
	{"os_in_use", "in_use"},
	{"os_servers", "servers"},
	{"os_server_limit", "server_limit"},
	{"os_grant_use", "grant_use"},
}

// renameNodeFields moves each old key to its new name in every node that still
// has it. Only rows carrying the old key are touched, so it is a no-op on every
// start after the first.
func renameNodeFields(db *gorm.DB, log *zap.SugaredLogger) error {
	for _, f := range renamedNodeFields {
		// "data -> key IS NOT NULL" rather than jsonb's "?" operator, which
		// GORM would read as a placeholder.
		res := db.Exec(`UPDATE nodes SET data = (data - ?::text) || jsonb_build_object(?::text, data -> ?::text) WHERE data -> ?::text IS NOT NULL`,
			f[0], f[1], f[0], f[0])
		if res.Error != nil {
			return fmt.Errorf("rename node field %s: %w", f[0], res.Error)
		}
		if res.RowsAffected > 0 {
			log.Infow("Renamed a stored node field", "from", f[0], "to", f[1], "nodes", res.RowsAffected)
		}
	}
	return nil
}
