package nodestore

import (
	"os"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestNormalizeEndDates runs the migration against a real Postgres: this package has no test database, so it runs only with OSMGT_TEST_POSTGRES_DSN set (e.g. a throwaway container) and is skipped otherwise.
func TestNormalizeEndDates(t *testing.T) {
	dsn := os.Getenv("OSMGT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSMGT_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&nodeRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&nodeRow{}); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{
		"bare":    `{"termination_date":"2026-12-31"}`,
		"picker":  `{"termination_date":"2026-12-30T23:00:00.000Z"}`,
		"form":    `{"termination_date":"2026-12-31T00:00:00.000Z"}`,
		"term":    `{"termination_date":"2026-12-31T23:59:59Z","pending":{"termination_date":"2027-06-29T22:00:00.000Z","limit":{"cores":2}}}`,
		"offset":  `{"termination_date":"2027-06-30T01:00:00+02:00"}`,
		"none":    `{"name":"no end"}`,
		"broken":  `{"termination_date":"31.12.2026"}`,
		"history": `{"termination_date":"2026-12-31T00:00:00.000Z","history":[{"termination_date_to":"2026-12-31T00:00:00.000Z"}]}`,
	}
	for id, data := range rows {
		if err := db.Exec(`INSERT INTO nodes (id, kind, status, owner, admin_scope, eligible_requesters, data) VALUES (?, 'project', 'approved', 'x', '[]', '[]', ?::jsonb)`, id, data).Error; err != nil {
			t.Fatal(err)
		}
	}
	log := zap.NewNop().Sugar()
	for range 2 { // the second run must change nothing
		if err := normalizeEndDates(db, log); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id, path string) string {
		var v *string
		if err := db.Raw(`SELECT data #>> ?::text[] FROM nodes WHERE id = ?`, path, id).Scan(&v).Error; err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "<none>"
		}
		return *v
	}
	for id, want := range map[string]string{"bare": "2026-12-31", "picker": "2026-12-31", "form": "2026-12-31", "term": "2026-12-31", "offset": "2027-06-30", "none": "<none>", "broken": "31.12.2026", "history": "2026-12-31"} {
		if got := read(id, "{termination_date}"); got != want {
			t.Errorf("%s: termination_date %s, want %s", id, got, want)
		}
	}
	if got := read("term", "{pending,termination_date}"); got != "2027-06-30" {
		t.Errorf("pending end %s, want 2027-06-30", got)
	}
	if got := read("term", "{pending,limit,cores}"); got != "2" {
		t.Errorf("the rest of pending must stay, got cores %s", got)
	}
	if got := read("history", "{history,0,termination_date_to}"); got != "2026-12-31T00:00:00.000Z" {
		t.Errorf("history must stay as recorded, got %s", got)
	}
}
