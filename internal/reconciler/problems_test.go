package reconciler

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestProblems_MergedOverRecentRuns(t *testing.T) {
	rec := &problemRecorder{}
	log := withRecorder(zap.NewNop().Sugar(), rec)
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	// Run 1: a grant that cannot be revoked, logged twice, and an info line.
	rec.begin(t0)
	for range 2 {
		log.Warnw("Failed to sync availability", "node_id", "p1", "os_project_id", "os1",
			"resource", "dhbw-v4-net", "error", errors.New("409 in use (req-1)"))
	}
	log.Infow("Granted availability", "node_id", "p2")
	rec.end(t0.Add(time.Minute), nil)

	// Run 2: the same grant again with another request ID, plus a failure.
	rec.begin(t0.Add(5 * time.Minute))
	log.Warnw("Failed to sync availability", "node_id", "p1", "os_project_id", "os1",
		"resource", "dhbw-v4-net", "error", errors.New("409 in use (req-2)"))
	log.Errorw("Reconciliation failed", "error", errors.New("keystone down"))
	rec.end(t0.Add(6*time.Minute), errors.New("keystone down"))

	// Outside a run nothing is recorded.
	log.Warnw("between runs")

	runs, problems := rec.snapshot()
	if len(runs) != 2 || runs[0].Error != "keystone down" || runs[0].Problems != 2 || runs[1].Problems != 1 {
		t.Fatalf("runs = %+v, want newest first with 2 and 1 problems", runs)
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %+v, want 2 (info and between-runs lines left out)", problems)
	}
	var grant *Problem
	for i := range problems {
		if problems[i].Message == "Failed to sync availability" {
			grant = &problems[i]
		}
	}
	if grant == nil {
		t.Fatalf("grant problem missing: %+v", problems)
	}
	if grant.Runs != 2 || grant.NodeID != "p1" || grant.OSProjectID != "os1" ||
		grant.Fields["resource"] != "dhbw-v4-net" || grant.Error != "409 in use (req-2)" || grant.Level != "warn" {
		t.Errorf("grant problem = %+v, want both runs merged with the latest error", grant)
	}
}

func TestProblems_KeepsOnlyRecentRuns(t *testing.T) {
	rec := &problemRecorder{}
	log := withRecorder(zap.NewNop().Sugar(), rec)
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for i := range recentRuns + 3 {
		rec.begin(t0.Add(time.Duration(i) * time.Minute))
		if i == 0 {
			log.Warnw("only in the first run")
		}
		rec.end(t0.Add(time.Duration(i)*time.Minute+time.Second), nil)
	}
	runs, problems := rec.snapshot()
	if len(runs) != recentRuns {
		t.Errorf("kept %d runs, want %d", len(runs), recentRuns)
	}
	if len(problems) != 0 {
		t.Errorf("problems = %+v, want the first run's problem gone with it", problems)
	}
}

func TestProblems_NilRecorder(t *testing.T) {
	var rec *problemRecorder
	rec.begin(time.Now())
	rec.end(time.Now(), nil)
	runs, problems := rec.snapshot()
	if runs == nil || problems == nil || len(runs)+len(problems) != 0 {
		t.Errorf("nil recorder must report empty lists, got %v %v", runs, problems)
	}
}
