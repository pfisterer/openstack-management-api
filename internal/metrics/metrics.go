// Package metrics is what GET /metrics shows (golib's promtext, the Prometheus text format by hand): the reconciler's last run and its counts, and the panics recovered in the background loops.
package metrics

import (
	"reflect"
	"strings"
	"unicode"

	"github.com/pfisterer/cloud-self-service-golib/loop"
	"github.com/pfisterer/cloud-self-service-golib/promtext"
	"go.uber.org/zap"
)

// LoopPanics counts the panics recovered in a background loop, per loop.
var LoopPanics = promtext.NewCounterVec("osmgt_loop_panics_total", "Panics recovered in a background loop of the OpenStack management API, per loop; the loop went on at its next tick.", "loop")

// Loop is the loop.Options of a named background loop: its log, and a panic counted in LoopPanics. The loop's series exists from the start at 0, so an alert on increase() sees the first panic.
func Loop(name string, log *zap.SugaredLogger) loop.Options {
	LoopPanics.Init(name)
	return loop.Options{Name: name, Log: log, OnPanic: func(any, []byte) { LoopPanics.Inc(name) }}
}

// Run is what the page shows of the reconciler's last run.
type Run struct {
	// Enabled: the reconciler is switched on; Connected: it has an OpenStack connection (it retries while not).
	Enabled, Connected bool
	// LastRunUnix is when the last run ended, 0 = none yet; LastFailed: it ended with an error.
	LastRunUnix float64
	LastFailed  bool
	// Counts are the counters of the last successful run (reconciler.Counters), any struct of int fields; each becomes a gauge osmgt_reconciler_last_run_<json name>.
	Counts any
}

// Write writes the reconciler's gauges (when run is not nil) and the loop panics.
func Write(w *promtext.Writer, run *Run) {
	if run != nil {
		gauge(w, "osmgt_reconciler_enabled", "1 when the reconciler is switched on (RECONCILER_ENABLED).", run.Enabled)
		gauge(w, "osmgt_reconciler_connected", "1 when the reconciler has a connection to OpenStack; it keeps retrying while 0.", run.Connected)
		w.Header("osmgt_reconciler_last_run_timestamp_seconds", "gauge", "When the reconciler's last run ended (Unix time); 0 before the first.")
		w.Sample("osmgt_reconciler_last_run_timestamp_seconds", run.LastRunUnix)
		gauge(w, "osmgt_reconciler_last_run_failed", "1 when the reconciler's last run ended with an error (the status page and the log say which).", run.LastFailed)
		writeCounts(w, run.Counts)
	}
	LoopPanics.WriteTo(w)
}

func gauge(w *promtext.Writer, name, help string, v bool) {
	w.Header(name, "gauge", help)
	f := 0.0
	if v {
		f = 1
	}
	w.Sample(name, f)
}

// writeCounts writes every int field of counts with a JSON name as a gauge; fields tagged "-" are left out.
func writeCounts(w *promtext.Writer, counts any) {
	v := reflect.Indirect(reflect.ValueOf(counts))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return
	}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "" || name == "-" || !v.Field(i).CanInt() {
			continue
		}
		metric := "osmgt_reconciler_last_run_" + name
		w.Header(metric, "gauge", "What the reconciler's last successful run counted: "+words(f.Name)+".")
		w.Sample(metric, float64(v.Field(i).Int()))
	}
}

// words is a field name in words: ProjectsSynced → projects synced.
func words(name string) string {
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte(' ')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
