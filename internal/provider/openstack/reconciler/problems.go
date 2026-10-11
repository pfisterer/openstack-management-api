package reconciler

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// What went wrong in the last few runs, for the admin view. A failed grant, a
// quota Nova refused or a purge stage that keeps failing is logged as a warning
// and the run goes on — correct for the reconciler, but nobody reads the log, so
// the portal shows "revoked" while OpenStack still grants. Recording what is
// LOGGED rather than adding a report call at every failure keeps the two from
// drifting apart: every warning there is, and every one added later, shows up.

// recentRuns is how many runs the admin view looks back over. With a run every
// few minutes that is the better part of an hour; follow-up passes while a
// project is emptied shorten it.
const recentRuns = 10

// Problem is one warning or error, merged over the recent runs it occurred in.
type Problem struct {
	Level       string `json:"level"`
	Message     string `json:"message"`
	NodeID      string `json:"node_id,omitempty"`
	OSProjectID string `json:"os_project_id,omitempty"`
	// Error is the text of the most recent occurrence; it is not part of what
	// makes two problems the same, a changing request ID would split them.
	Error string `json:"error,omitempty"`
	// Fields are the remaining log fields (resource, stage, …) as text.
	Fields map[string]string `json:"fields,omitempty"`
	// Runs counts the recent runs it occurred in, out of RecentRuns: one of ten
	// is a hiccup, ten of ten is broken.
	Runs      int       `json:"runs"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// RunSummary is one recent run.
type RunSummary struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
	Problems   int       `json:"problems"`
}

type runProblems struct {
	summary  RunSummary
	problems []Problem
}

// problemRecorder keeps the problems of the run in progress and of the recent
// finished ones. Only what is logged during a run counts: outside one there is
// no run to attribute it to.
type problemRecorder struct {
	mu      sync.Mutex
	current *runProblems
	index   map[string]int
	done    []runProblems // oldest first, at most recentRuns
}

// The methods accept a nil recorder: a Reconciler built in a test without New
// has none and records nothing.

func (p *problemRecorder) begin(now time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = &runProblems{summary: RunSummary{StartedAt: now}}
	p.index = map[string]int{}
}

func (p *problemRecorder) end(now time.Time, runErr error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return
	}
	p.current.summary.FinishedAt = now
	if runErr != nil {
		p.current.summary.Error = runErr.Error()
	}
	p.current.summary.Problems = len(p.current.problems)
	p.done = append(p.done, *p.current)
	if len(p.done) > recentRuns {
		p.done = p.done[len(p.done)-recentRuns:]
	}
	p.current, p.index = nil, nil
}

func (p *problemRecorder) record(e zapcore.Entry, fields []zapcore.Field) {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	pr := Problem{Level: e.Level.String(), Message: e.Message, LastSeen: e.Time, FirstSeen: e.Time}
	for k, v := range enc.Fields {
		s := fieldText(v)
		switch {
		case k == "node_id":
			pr.NodeID = s
		case k == "os_project_id":
			pr.OSProjectID = s
		case k == "error":
			pr.Error = s
		case strings.HasSuffix(k, "Verbose"):
			// zap's stack-carrying twin of "error"; the table has no room for it.
		default:
			if pr.Fields == nil {
				pr.Fields = map[string]string{}
			}
			pr.Fields[k] = s
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return
	}
	key := pr.key()
	if i, ok := p.index[key]; ok {
		// Within a run the same problem counts once; the latest text wins.
		p.current.problems[i].Error = pr.Error
		p.current.problems[i].LastSeen = pr.LastSeen
		return
	}
	p.index[key] = len(p.current.problems)
	p.current.problems = append(p.current.problems, pr)
}

// key is what makes two log lines the same problem.
func (pr Problem) key() string {
	parts := []string{pr.Level, pr.Message, pr.NodeID, pr.OSProjectID}
	keys := make([]string, 0, len(pr.Fields))
	for k := range pr.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+pr.Fields[k])
	}
	return strings.Join(parts, "\x00")
}

// fieldText renders a log field for the table, cut so one huge value cannot
// swamp it.
func fieldText(v any) string {
	s := fmt.Sprint(v)
	const max = 500
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// snapshot merges the finished runs, newest problems first. The run in
// progress is left out: half a run would show problems as fixed that it has
// not reached yet.
func (p *problemRecorder) snapshot() ([]RunSummary, []Problem) {
	if p == nil {
		return []RunSummary{}, []Problem{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	runs := make([]RunSummary, 0, len(p.done))
	merged := map[string]*Problem{}
	var order []string
	for _, run := range p.done {
		runs = append(runs, run.summary)
		for _, pr := range run.problems {
			k := pr.key()
			m := merged[k]
			if m == nil {
				c := pr
				c.Runs = 0
				merged[k] = &c
				order = append(order, k)
				m = &c
			}
			m.Runs++
			m.Error = pr.Error
			m.LastSeen = pr.LastSeen
		}
	}
	slices.Reverse(runs)
	problems := make([]Problem, 0, len(order))
	for _, k := range order {
		problems = append(problems, *merged[k])
	}
	// Still happening first, then the most persistent.
	sort.SliceStable(problems, func(i, j int) bool {
		if !problems[i].LastSeen.Equal(problems[j].LastSeen) {
			return problems[i].LastSeen.After(problems[j].LastSeen)
		}
		return problems[i].Runs > problems[j].Runs
	})
	return runs, problems
}

// recordingCore hands every warning and error to the recorder, next to wherever
// the log goes anyway.
type recordingCore struct {
	rec    *problemRecorder
	fields []zapcore.Field
}

func (c *recordingCore) Enabled(l zapcore.Level) bool { return l >= zapcore.WarnLevel }

func (c *recordingCore) With(fields []zapcore.Field) zapcore.Core {
	return &recordingCore{rec: c.rec, fields: append(slices.Clip(c.fields), fields...)}
}

func (c *recordingCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *recordingCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	c.rec.record(e, append(slices.Clip(c.fields), fields...))
	return nil
}

func (c *recordingCore) Sync() error { return nil }

// withRecorder returns log with rec listening in; the log itself is unchanged.
func withRecorder(log *zap.SugaredLogger, rec *problemRecorder) *zap.SugaredLogger {
	return log.Desugar().WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return zapcore.NewTee(core, &recordingCore{rec: rec})
	})).Sugar()
}
