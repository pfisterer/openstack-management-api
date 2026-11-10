package nodestore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Decided 10.11.2026 (docs/plan-caps-katalog.md): the four budget settings of the AI service became caps the provider declares (common.Cap), stored as one map "caps" on the node and as "caps_from"/"caps_to" in the history; and a member's role is "role" instead of "openstack_role". migrateCaps rewrites the stored nodes once. The ids are those of the AI service's caps at the time (aiaas.Caps; a test there holds them to LegacyCapIDs).

// The cap ids the four former settings became.
const (
	capReleaseCPUIdleOpen   = "release-cpu-idle-open"
	capReleaseCPUIdleClosed = "release-cpu-idle-closed"
	capReleaseCPUMaxHours   = "release-cpu-max-hours"
	capReleaseGPUIdleOpen   = "release-gpu-idle-open"
	capReleaseGPUIdleClosed = "release-gpu-idle-closed"
	capReleaseGPUMaxHours   = "release-gpu-max-hours"
	capWorkspaceScratchGB   = "workspace-scratch-gb"
	capBatchPriority        = "batch-priority"
	capBatchJobsPerPerson   = "batch-jobs-per-person"
	capBatchMaxHours        = "batch-max-hours"
)

// LegacyCapIDs are the caps the migration writes.
var LegacyCapIDs = []string{capReleaseCPUIdleOpen, capReleaseCPUIdleClosed, capReleaseCPUMaxHours, capReleaseGPUIdleOpen, capReleaseGPUIdleClosed, capReleaseGPUMaxHours, capWorkspaceScratchGB, capBatchPriority, capBatchJobsPerPerson, capBatchMaxHours}

// The batch priorities as indexes of the cap's choices, strictest first.
var batchPriorityIndex = map[string]int{"preemptible": 0, "guaranteed": 1}

// legacyCaps are the four former settings as caps, each read from its JSON value; a setting missing or null gives nothing.
var legacyCaps = []struct {
	key  string
	caps func(v any) map[string]int
}{
	{"release_max", releaseCaps},
	{"workspace_scratch_gb", func(v any) map[string]int { return intCap(capWorkspaceScratchGB, v) }},
	{"batch_priority", func(v any) map[string]int {
		if s, ok := v.(string); ok {
			if i, ok := batchPriorityIndex[s]; ok {
				return map[string]int{capBatchPriority: i}
			}
		}
		return nil
	}},
	{"batch_jobs", func(v any) map[string]int {
		m, _ := v.(map[string]any)
		jobs, hours := intCap(capBatchJobsPerPerson, m["jobs_per_person"]), intCap(capBatchMaxHours, m["max_hours"])
		if len(jobs) == 0 || len(hours) == 0 || jobs[capBatchJobsPerPerson] <= 0 || hours[capBatchMaxHours] <= 0 {
			return nil // off
		}
		maps.Copy(jobs, hours)
		return jobs
	}},
}

func releaseCaps(v any) map[string]int {
	m, _ := v.(map[string]any)
	out := map[string]int{}
	for _, kind := range []struct {
		key                      string
		open, closed, upperBound string
	}{{"cpu", capReleaseCPUIdleOpen, capReleaseCPUIdleClosed, capReleaseCPUMaxHours}, {"gpu", capReleaseGPUIdleOpen, capReleaseGPUIdleClosed, capReleaseGPUMaxHours}} {
		t, _ := m[kind.key].(map[string]any)
		maps.Copy(out, intCap(kind.open, t["idle_open_minutes"]))
		maps.Copy(out, intCap(kind.closed, t["idle_closed_minutes"]))
		maps.Copy(out, intCap(kind.upperBound, t["max_hours"]))
	}
	return out
}

func intCap(id string, v any) map[string]int {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	return map[string]int{id: int(i)}
}

// migrateNodeData rewrites one node's data: the four settings into "caps", their history fields into "caps_from"/"caps_to" (only the ids that differ, as the tree writes them now), "openstack_role" of the members and of a waiting change into "role". It reports whether anything changed.
func migrateNodeData(data map[string]any) bool {
	changed := false
	own := map[string]int{}
	for _, l := range legacyCaps {
		v, ok := data[l.key]
		if !ok {
			continue
		}
		delete(data, l.key)
		changed = true
		maps.Copy(own, l.caps(v))
	}
	if len(own) > 0 {
		caps, _ := data["caps"].(map[string]any)
		if caps == nil {
			caps = map[string]any{}
		}
		for id, v := range own {
			caps[id] = v
		}
		data["caps"] = caps
	}
	if history, ok := data["history"].([]any); ok {
		for _, e := range history {
			if entry, ok := e.(map[string]any); ok && migrateHistoryEntry(entry) {
				changed = true
			}
		}
	}
	if renameRoles(data["authorized_users"]) {
		changed = true
	}
	if pending, ok := data["pending"].(map[string]any); ok && renameRoles(pending["authorized_users"]) {
		changed = true
	}
	return changed
}

func migrateHistoryEntry(entry map[string]any) bool {
	from, to := map[string]int{}, map[string]int{}
	changed := false
	for _, l := range legacyCaps {
		f, hasFrom := entry[l.key+"_from"]
		t, hasTo := entry[l.key+"_to"]
		if !hasFrom && !hasTo {
			continue
		}
		delete(entry, l.key+"_from")
		delete(entry, l.key+"_to")
		changed = true
		a, b := l.caps(f), l.caps(t)
		for id, v := range a {
			if w, ok := b[id]; !ok || w != v {
				from[id] = v
			}
		}
		for id, v := range b {
			if w, ok := a[id]; !ok || w != v {
				to[id] = v
			}
		}
	}
	if len(from) > 0 {
		entry["caps_from"] = from
	}
	if len(to) > 0 {
		entry["caps_to"] = to
	}
	return changed
}

func renameRoles(v any) bool {
	list, _ := v.([]any)
	changed := false
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := m["openstack_role"]; ok {
			if _, has := m["role"]; !has {
				m["role"] = r
			}
			delete(m, "openstack_role")
			changed = true
		}
	}
	return changed
}

// migrateCaps rewrites every node that still holds one of the former fields (migrateNodeData), each under a guard on the data it read, so it is a no-op on every start after the first and cannot overwrite a change made meanwhile (that node is rewritten at the next start).
func migrateCaps(db *gorm.DB, log *zap.SugaredLogger) error {
	keys := []string{"openstack_role"}
	for _, l := range legacyCaps {
		keys = append(keys, l.key)
	}
	var where []string
	var args []any
	for _, k := range keys {
		where = append(where, "data::text LIKE ?")
		args = append(args, `%"`+k+`%`)
	}
	var rows []struct {
		ID   string
		Data string
	}
	if err := db.Raw(`SELECT id, data::text AS data FROM nodes WHERE `+strings.Join(where, " OR "), args...).Scan(&rows).Error; err != nil {
		return fmt.Errorf("read nodes: %w", err)
	}
	changed := 0
	for _, r := range rows {
		dec := json.NewDecoder(bytes.NewReader([]byte(r.Data)))
		dec.UseNumber()
		var data map[string]any
		if err := dec.Decode(&data); err != nil {
			log.Warnw("Node data cannot be read, caps not migrated", "node", r.ID, "error", err)
			continue
		}
		if !migrateNodeData(data) {
			continue
		}
		next, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("write node %s: %w", r.ID, err)
		}
		res := db.Exec(`UPDATE nodes SET data = ?::jsonb WHERE id = ? AND data = ?::jsonb`, string(next), r.ID, r.Data)
		if res.Error != nil {
			return fmt.Errorf("write node %s: %w", r.ID, res.Error)
		}
		if res.RowsAffected > 0 {
			changed++
		}
	}
	if changed > 0 {
		log.Infow("Budget settings written as caps, member roles as role", "nodes", changed)
	}
	return nil
}
