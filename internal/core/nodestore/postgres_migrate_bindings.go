package nodestore

import (
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// boundNodeFields are keys of the stored node document that moved into the
// binding of the one provider there was when they were written: a leaf can
// now be carried out by several providers, each with its own project ID and
// measurements.
var boundNodeFields = []string{
	"external_id", "external_name", "overcommitted", "in_use", "servers",
	"server_limit", "grant_use", "purge", "external_group_assignments",
}

// bindNodeFields moves those keys into data.bindings.openstack in every node
// that still has one of them at the top. Keys already in the binding win over
// the old ones, so a half-done run cannot overwrite newer values. A no-op on
// every start after the first.
func bindNodeFields(db *gorm.DB, log *zap.SugaredLogger) error {
	var removes, pairs, present []string
	var removeArgs, pairArgs, presentArgs []any
	for _, f := range boundNodeFields {
		removes = append(removes, " - ?::text")
		removeArgs = append(removeArgs, f)
		pairs = append(pairs, "?::text, data -> ?::text")
		pairArgs = append(pairArgs, f, f)
		// "data -> key IS NOT NULL" rather than jsonb's "?" operator, which
		// GORM would read as a placeholder.
		present = append(present, "data -> ?::text IS NOT NULL")
		presentArgs = append(presentArgs, f)
	}
	sql := `UPDATE nodes SET data = (data` + strings.Join(removes, "") + `) || jsonb_build_object('bindings',
		COALESCE(data -> 'bindings', '{}'::jsonb) || jsonb_build_object('openstack',
			jsonb_strip_nulls(jsonb_build_object(` + strings.Join(pairs, ", ") + `))
			|| COALESCE(data -> 'bindings' -> 'openstack', '{}'::jsonb)))
		WHERE ` + strings.Join(present, " OR ")
	res := db.Exec(sql, slices.Concat(removeArgs, pairArgs, presentArgs)...)
	if res.Error != nil {
		return fmt.Errorf("move node fields into the openstack binding: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infow("Moved stored node fields into the openstack binding", "nodes", res.RowsAffected)
	}
	return nil
}

// factBindingFields are the counts that moved, inside the openstack binding,
// from keys of their own into its facts.
var factBindingFields = []string{"servers", "server_limit"}

// factBindingCounts moves them into data.bindings.openstack.facts in every
// node that still has one of them. Facts already there win. Runs after
// bindNodeFields, which brings the oldest nodes' counts into the binding
// first. A no-op on every start after the first.
func factBindingCounts(db *gorm.DB, log *zap.SugaredLogger) error {
	var pairs, removes, present []string
	var pairArgs, removeArgs []any
	for _, f := range factBindingFields {
		pairs = append(pairs, "?::text, data #> ARRAY['bindings', 'openstack', ?::text]")
		pairArgs = append(pairArgs, f, f)
		removes = append(removes, " - ?::text")
		removeArgs = append(removeArgs, f)
		present = append(present, "data #> ARRAY['bindings', 'openstack', '"+f+"'] IS NOT NULL")
	}
	sql := `UPDATE nodes SET data = jsonb_set(data, '{bindings,openstack}',
		((data #> '{bindings,openstack}')` + strings.Join(removes, "") + `) || jsonb_build_object('facts',
			jsonb_strip_nulls(jsonb_build_object(` + strings.Join(pairs, ", ") + `))
			|| COALESCE(data #> '{bindings,openstack,facts}', '{}'::jsonb)))
		WHERE ` + strings.Join(present, " OR ")
	res := db.Exec(sql, slices.Concat(removeArgs, pairArgs)...)
	if res.Error != nil {
		return fmt.Errorf("move server counts into the openstack binding's facts: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infow("Moved server counts into the openstack binding's facts", "nodes", res.RowsAffected)
	}
	return nil
}
