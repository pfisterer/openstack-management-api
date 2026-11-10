package nodestore

import (
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// normalizeEndDates writes every stored end date as a day, "2006-01-02" (decided 10.11.2026, model.EndDay): the node's termination_date and that of a waiting request (pending.termination_date). The history keeps what it recorded. Only rows with another spelling are touched, each under a guard on the value it read, so it is a no-op on every start after the first and cannot overwrite a change made meanwhile. An end that cannot be read stays as it is and is logged. The OpenStack tags follow at the reconciler's next pass, which writes a tag when it differs.
func normalizeEndDates(db *gorm.DB, log *zap.SugaredLogger) error {
	for _, f := range []struct{ name, path, read string }{
		{"termination_date", "{termination_date}", "data ->> 'termination_date'"},
		{"pending.termination_date", "{pending,termination_date}", "data -> 'pending' ->> 'termination_date'"},
	} {
		var rows []struct {
			ID  string
			End string
		}
		if err := db.Raw(`SELECT id, ` + f.read + ` AS "end" FROM nodes WHERE ` + f.read + ` IS NOT NULL AND ` + f.read + ` !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'`).Scan(&rows).Error; err != nil {
			return fmt.Errorf("read %s: %w", f.name, err)
		}
		changed := 0
		for _, r := range rows {
			d, err := model.EndDay(r.End)
			if err != nil {
				log.Warnw("Stored end date cannot be read, left as it is", "node", r.ID, "field", f.name, "value", r.End)
				continue
			}
			res := db.Exec(`UPDATE nodes SET data = jsonb_set(data, ?::text[], to_jsonb(?::text)) WHERE id = ? AND `+f.read+` = ?`, f.path, d.String(), r.ID, r.End)
			if res.Error != nil {
				return fmt.Errorf("write %s of %s: %w", f.name, r.ID, res.Error)
			}
			if res.RowsAffected > 0 {
				changed++
				log.Infow("Stored end date written as a day", "node", r.ID, "field", f.name, "from", r.End, "to", d.String())
			}
		}
		if changed > 0 {
			log.Infow("Stored end dates written as days", "field", f.name, "nodes", changed)
		}
	}
	return nil
}
