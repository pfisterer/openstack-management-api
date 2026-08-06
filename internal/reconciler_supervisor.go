package app

import (
	"cmp"
	"context"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// reconcilerSupervisor keeps the reconciler's slot filled while the OpenStack
// connection is being established, and hands the real one over once it is.
//
// It exists because the startup used to treat a network error as permanent: if
// OpenStack was unreachable in the second the process happened to start, the
// reconciler was disabled for the lifetime of the pod, with nothing to retry it
// and nothing to say so. The running loop meanwhile shrugs the same error off
// and carries on — the two halves disagreed about whether a dial failure means
// anything, and the startup half was wrong. On 2026-08-06 a ten-minute outage
// of the cloud turned into a fifty-minute one for exactly that reason, and it
// ended only because a deployment happened to restart the pod.
//
// The webserver holds this from the start, so /v1/config and the admin routes
// can answer honestly ("connecting" rather than "disabled") instead of the API
// having to be rewired once the connection succeeds.
type reconcilerSupervisor struct {
	// inner is nil until the first successful connection. Read on every API
	// request, written once by the retry loop.
	inner atomic.Pointer[reconciler.Reconciler]

	// Backoff bounds, overridable so tests do not have to wait out a real one.
	// Zero means the package defaults.
	minDelay, maxDelay time.Duration
}

var _ webserver.ReconcilerAPI = (*reconcilerSupervisor)(nil)

func (s *reconcilerSupervisor) Ready() bool { return s.inner.Load() != nil }

func (s *reconcilerSupervisor) Trigger() {
	if rec := s.inner.Load(); rec != nil {
		rec.Trigger()
	}
}

// GetStatus reports an empty status with an explanation while connecting, so a
// caller can tell "not up yet" from "ran and found nothing".
func (s *reconcilerSupervisor) GetStatus() reconciler.Status {
	if rec := s.inner.Load(); rec != nil {
		return rec.GetStatus()
	}
	return reconciler.Status{LastError: "connecting to OpenStack — no run has completed yet"}
}

// backoff bounds for the connection retry. The floor is short because the usual
// case is a cloud that is briefly away; the ceiling keeps a long outage from
// hammering it, and the jitter keeps several pods from retrying in lockstep.
const (
	reconnectMinDelay = 15 * time.Second
	reconnectMaxDelay = 5 * time.Minute
)

// connectAndStart dials OpenStack until it works, then builds the reconciler and
// starts it. It returns immediately; the work happens in a goroutine that lives
// until ctx is cancelled.
//
// build is what needs a live connection — it is passed in so this file stays out
// of the wiring details in RunApplication.
func (s *reconcilerSupervisor) connectAndStart(ctx context.Context, build func() (*reconciler.Reconciler, error), log *zap.SugaredLogger) {
	go func() {
		if rec := s.connectWithRetry(ctx, build, log); rec != nil {
			rec.Start(ctx) // blocks until ctx is done
		}
	}()
}

// connectWithRetry loops until build succeeds or ctx ends, publishing the result
// so the API becomes Ready. It returns nil when the context ended first.
//
// Kept separate from starting the reconciler so the retry behaviour can be
// tested without a running reconciler behind it.
func (s *reconcilerSupervisor) connectWithRetry(ctx context.Context, build func() (*reconciler.Reconciler, error), log *zap.SugaredLogger) *reconciler.Reconciler {
	minDelay := cmp.Or(s.minDelay, reconnectMinDelay)
	maxDelay := cmp.Or(s.maxDelay, reconnectMaxDelay)

	delay := minDelay
	for attempt := 1; ; attempt++ {
		rec, err := build()
		if err == nil {
			s.inner.Store(rec)
			if attempt > 1 {
				log.Infow("OpenStack reachable — starting reconciler", "attempts", attempt)
			}
			return rec
		}

		// Every attempt is logged: a reconciler that is not running is worth
		// being loud about, and the interval is already capped.
		log.Warnw("OpenStack not reachable — retrying, reconciler not running yet",
			"attempt", attempt, "retry_in", delay.String(), zap.Error(err))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(delay)):
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// jitter spreads retries by ±10% so restarts do not synchronise on the cloud.
func jitter(d time.Duration) time.Duration {
	spread := float64(d) * 0.1
	return d + time.Duration((rand.Float64()*2-1)*spread)
}
