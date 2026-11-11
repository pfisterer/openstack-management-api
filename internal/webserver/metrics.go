package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/promtext"
	"github.com/pfisterer/openstack-management-api/internal/metrics"
)

// MetricsRouter serves GET /metrics in the Prometheus text format: the reconciler's last run (when it ended, whether it failed, what it counted) and the panics recovered in the background loops. rec is nil when the reconciler is switched off.
//
// Its own listener (METRICS_BIND), without a token, as in the AI service: the ingress points at the API's port only, so the page is not reachable from outside the cluster. The series carry counts and a time, nothing about a person or a project.
func MetricsRouter(rec ReconcilerAPI, enabled bool) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/metrics", func(c *gin.Context) {
		var w promtext.Writer
		metrics.Write(&w, reconcilerRun(rec, enabled))
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", w.Bytes())
	})
	return r
}

// reconcilerRun is what the metrics show of the reconciler; nil without one.
func reconcilerRun(rec ReconcilerAPI, enabled bool) *metrics.Run {
	if !enabled {
		return nil
	}
	run := &metrics.Run{Enabled: true}
	if rec == nil || !rec.Ready() {
		return run
	}
	st := rec.GetStatus()
	run.Connected = true
	if !st.LastRunAt.IsZero() {
		run.LastRunUnix = float64(st.LastRunAt.Unix())
	}
	run.LastFailed = st.LastError != ""
	run.Counts = st.Counters
	return run
}
