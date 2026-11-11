package reconciler

import (
	"cmp"
	"context"
	"sync/atomic"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/retry"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/catalog"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// Supervisor keeps the reconciler's slot filled while the OpenStack
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
type Supervisor struct {
	// inner is nil until the first successful connection. Read on every API
	// request, written once by the retry loop.
	inner atomic.Pointer[Reconciler]

	// Backoff bounds, overridable so tests do not have to wait out a real one.
	// Zero means the package defaults.
	minDelay, maxDelay time.Duration
}

// It answers the catalogue's questions to OpenStack, too.
var _ catalog.OpenStack = (*Supervisor)(nil)

func (s *Supervisor) Ready() bool { return s.inner.Load() != nil }

func (s *Supervisor) Trigger() {
	if rec := s.inner.Load(); rec != nil {
		rec.Trigger()
	}
}

// GetStatus reports an empty status with an explanation while connecting, so a
// caller can tell "not up yet" from "ran and found nothing".
func (s *Supervisor) GetStatus() Status {
	if rec := s.inner.Load(); rec != nil {
		return rec.GetStatus()
	}
	return Status{LastError: "connecting to OpenStack — no run has completed yet"}
}

// CheckGrantTarget and GrantedProjects let root admins ask the cloud about an
// availability (package catalog); until connected there is nobody to ask.
func (s *Supervisor) CheckGrantTarget(g osclient.Grant) error {
	if rec := s.inner.Load(); rec != nil {
		return rec.CheckGrantTarget(g)
	}
	return catalog.ErrUnavailable
}

func (s *Supervisor) GrantedProjects(g osclient.Grant) ([]string, error) {
	if rec := s.inner.Load(); rec != nil {
		return rec.GrantedProjects(g)
	}
	return nil, catalog.ErrUnavailable
}

// backoff bounds for the connection retry. The floor is short because the usual
// case is a cloud that is briefly away; the ceiling keeps a long outage from
// hammering it, and the jitter (±10%) keeps several pods from retrying in lockstep.
const (
	reconnectMinDelay = 15 * time.Second
	reconnectMaxDelay = 5 * time.Minute
	reconnectJitter   = 0.1
)

// connectAndStart dials OpenStack until it works, then builds the reconciler and
// starts it. It returns immediately; the work happens in a goroutine that lives
// until ctx is cancelled.
//
// build is what needs a live connection — it is passed in so this file stays out
// of the wiring details in RunApplication.
func (s *Supervisor) ConnectAndStart(ctx context.Context, build func() (*Reconciler, error), log *zap.SugaredLogger) {
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
func (s *Supervisor) connectWithRetry(ctx context.Context, build func() (*Reconciler, error), log *zap.SugaredLogger) *Reconciler {
	var rec *Reconciler
	attempts := 0
	err := retry.Do(ctx, retry.Policy{
		Total:   -1, // until it works or ctx ends
		Initial: cmp.Or(s.minDelay, reconnectMinDelay),
		Max:     cmp.Or(s.maxDelay, reconnectMaxDelay),
		Jitter:  reconnectJitter,
		// Every attempt is logged: a reconciler that is not running is worth
		// being loud about, and the interval is already capped.
		OnRetry: func(attempt int, err error, wait time.Duration) {
			log.Warnw("OpenStack not reachable — retrying, reconciler not running yet",
				"attempt", attempt, "retry_in", wait.String(), zap.Error(err))
		},
	}, func(context.Context) error {
		attempts++
		r, err := build()
		if err != nil {
			return err
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil // ctx ended first
	}
	s.inner.Store(rec)
	if attempts > 1 {
		log.Infow("OpenStack reachable — starting reconciler", "attempts", attempts)
	}
	return rec
}
