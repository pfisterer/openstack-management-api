package nodestore

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func canonical(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(decode(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cases are shaped after the budgets on prod on 10.11.2026.
func TestMigrateNodeData(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"root with all but the priority",
			`{"name":"root","batch_jobs":{"max_hours":24,"jobs_per_person":4},"release_max":{"cpu":{"max_hours":12,"idle_open_minutes":120,"idle_closed_minutes":120},"gpu":{"max_hours":4,"idle_open_minutes":30,"idle_closed_minutes":5}},"workspace_scratch_gb":10,"history":[{"event":"updated","status_to":"approved","batch_jobs_to":{"max_hours":24,"jobs_per_person":4}},{"event":"updated","status_to":"approved","release_max_to":{"cpu":{"max_hours":12,"idle_open_minutes":120,"idle_closed_minutes":120},"gpu":{"max_hours":4,"idle_open_minutes":30,"idle_closed_minutes":5}},"workspace_scratch_gb_to":10}]}`,
			`{"name":"root","caps":{"batch-jobs-per-person":4,"batch-max-hours":24,"release-cpu-idle-open":120,"release-cpu-idle-closed":120,"release-cpu-max-hours":12,"release-gpu-idle-open":30,"release-gpu-idle-closed":5,"release-gpu-max-hours":4,"workspace-scratch-gb":10},"history":[{"event":"updated","status_to":"approved","caps_to":{"batch-jobs-per-person":4,"batch-max-hours":24}},{"event":"updated","status_to":"approved","caps_to":{"release-cpu-idle-open":120,"release-cpu-idle-closed":120,"release-cpu-max-hours":12,"release-gpu-idle-open":30,"release-gpu-idle-closed":5,"release-gpu-max-hours":4,"workspace-scratch-gb":10}}]}`,
		},
		{
			"priority changed back and forth, inheriting",
			`{"inherits_limit":true,"batch_priority":"guaranteed","history":[{"event":"updated","batch_priority_to":"guaranteed"},{"event":"updated","batch_priority_to":"preemptible","batch_priority_from":"guaranteed"},{"event":"updated","batch_priority_to":"guaranteed","batch_priority_from":"preemptible"}]}`,
			`{"inherits_limit":true,"caps":{"batch-priority":1},"history":[{"event":"updated","caps_to":{"batch-priority":1}},{"event":"updated","caps_from":{"batch-priority":1},"caps_to":{"batch-priority":0}},{"event":"updated","caps_from":{"batch-priority":0},"caps_to":{"batch-priority":1}}]}`,
		},
		{
			"jobs lowered, only the differing id recorded; limit entry kept",
			`{"batch_jobs":{"max_hours":8,"jobs_per_person":1},"batch_priority":"guaranteed","history":[{"event":"updated","limit_to":{"cores":2},"limit_from":{"cores":1},"batch_priority_to":"guaranteed"},{"event":"updated","batch_jobs_to":{"max_hours":8,"jobs_per_person":1},"batch_jobs_from":{"max_hours":8,"jobs_per_person":2}}]}`,
			`{"caps":{"batch-jobs-per-person":1,"batch-max-hours":8,"batch-priority":1},"history":[{"event":"updated","limit_to":{"cores":2},"limit_from":{"cores":1},"caps_to":{"batch-priority":1}},{"event":"updated","caps_from":{"batch-jobs-per-person":2},"caps_to":{"batch-jobs-per-person":1}}]}`,
		},
		{
			"release without upper bounds, unbounded GPU, batch jobs switched off",
			`{"release_max":{"cpu":{"idle_open_minutes":30,"idle_closed_minutes":30},"gpu":{"max_hours":0}},"history":[{"event":"updated","batch_jobs_from":{"max_hours":8,"jobs_per_person":2}}]}`,
			`{"caps":{"release-cpu-idle-open":30,"release-cpu-idle-closed":30,"release-gpu-max-hours":0},"history":[{"event":"updated","caps_from":{"batch-jobs-per-person":2,"batch-max-hours":8}}]}`,
		},
		{
			"roles of members and of a waiting change",
			`{"authorized_users":[{"token":"group:k","openstack_role":"reader"},{"token":"user:a@b.de","openstack_role":"member"}],"pending":{"authorized_users":[{"token":"user:c@b.de","openstack_role":"reader"}]}}`,
			`{"authorized_users":[{"token":"group:k","role":"reader"},{"token":"user:a@b.de","role":"member"}],"pending":{"authorized_users":[{"token":"user:c@b.de","role":"reader"}]}}`,
		},
	}
	for _, c := range cases {
		data := decode(t, c.in)
		if !migrateNodeData(data) {
			t.Errorf("%s: reported no change", c.name)
		}
		got, _ := json.Marshal(data)
		if string(got) != canonical(t, c.want) {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, canonical(t, c.want))
		}
		if migrateNodeData(data) {
			t.Errorf("%s: a second run changed something", c.name)
		}
	}
	if migrateNodeData(decode(t, `{"name":"x","caps":{"workspace-scratch-gb":5},"authorized_users":[{"token":"user:a@b.de","role":"member"}]}`)) {
		t.Error("a migrated node changed")
	}
}

// TestMigrateCapsPostgres opens a store on a copy of real data (OSMGT_TEST_CAPS_DSN, e.g. a throwaway container loaded with a dump of the nodes table; skipped otherwise): the migration runs on opening, and afterwards no node holds a former field, a second opening changes nothing, and every node still reads.
func TestMigrateCapsPostgres(t *testing.T) {
	dsn := os.Getenv("OSMGT_TEST_CAPS_DSN")
	if dsn == "" {
		t.Skip("OSMGT_TEST_CAPS_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	log := zap.NewNop().Sugar()
	for range 2 {
		if _, err := NewPostgresStore(dsn, log); err != nil {
			t.Fatal(err)
		}
	}
	var left int64
	if err := db.Raw(`SELECT count(*) FROM nodes WHERE data::text ~ '"(openstack_role|release_max|workspace_scratch_gb|batch_priority|batch_jobs)'`).Scan(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d nodes still hold a former field", left)
	}
	store, _ := NewPostgresStore(dsn, log)
	if _, err := store.ListNodes(t.Context(), NodeQuery{}, 0, 0); err != nil {
		t.Errorf("nodes do not read: %v", err)
	}
}
