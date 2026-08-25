package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"go.uber.org/zap"
)

// Before the connection succeeds the supervisor must answer honestly rather
// than pretend the reconciler is off: /v1/config drives whether provisioning
// appears in the UI at all, and a startup that gave up left it hidden for the
// life of the pod.
func TestReconcilerSupervisor_NotReadyBeforeConnected(t *testing.T) {
	s := &reconcilerSupervisor{}

	if s.Ready() {
		t.Error("Ready() = true before any connection")
	}
	if got := s.GetStatus().LastError; got == "" {
		t.Error("GetStatus() must say why it has nothing to report, got an empty status")
	}
	// Must not panic — the admin route can be called at any moment.
	s.Trigger()
}

// The retry keeps going instead of giving up on the first dial failure. This is
// the whole point: a cloud that is away for a minute must not disable
// provisioning until somebody restarts the pod.
func TestReconcilerSupervisor_RetriesUntilItConnects(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &reconcilerSupervisor{minDelay: 5 * time.Millisecond, maxDelay: 20 * time.Millisecond}
	go s.connectWithRetry(ctx, func() (*reconciler.Reconciler, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("dial tcp: i/o timeout")
		}
		// Returning nil here would make Ready() lie; a real build returns a
		// reconciler, so give it one. Start() on it is what blocks afterwards.
		return &reconciler.Reconciler{}, nil
	}, zap.NewNop().Sugar())

	deadline := time.After(5 * time.Second)
	for !s.Ready() {
		select {
		case <-deadline:
			t.Fatalf("never connected; attempts = %d", attempts.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if got := attempts.Load(); got < 3 {
		t.Errorf("attempts = %d, want at least 3 — it stopped retrying too early", got)
	}
}

// Cancelling must end the retry loop; otherwise a shutdown leaves a goroutine
// dialling an unreachable cloud forever.
func TestReconcilerSupervisor_StopsOnContextCancel(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	s := &reconcilerSupervisor{minDelay: 5 * time.Millisecond, maxDelay: 20 * time.Millisecond}
	// Wait for the loop to return rather than sampling the counter around
	// cancel(): the backoff timer can fire in the same instant, so an attempt
	// already inside build() gets counted after the cancel through no fault of
	// the code. Returning at all is the property under test, and once it has
	// returned the count is settled.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.connectWithRetry(ctx, func() (*reconciler.Reconciler, error) {
			attempts.Add(1)
			return nil, errors.New("dial tcp: i/o timeout")
		}, zap.NewNop().Sugar())
	}()

	// One attempt happens immediately; then it waits out the backoff.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("still retrying 5s after cancel; attempts = %d", attempts.Load())
	}

	after := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != after {
		t.Errorf("attempts kept rising after the loop returned (%d → %d)", after, got)
	}
	if s.Ready() {
		t.Error("Ready() = true although no connection ever succeeded")
	}
}

// The delay grows and then stops growing, so a long outage is retried at a
// bounded rate rather than hammering the cloud or drifting into hours.
func TestReconnectBackoffIsBounded(t *testing.T) {
	if reconnectMinDelay <= 0 || reconnectMinDelay > reconnectMaxDelay {
		t.Fatalf("bad bounds: min %v, max %v", reconnectMinDelay, reconnectMaxDelay)
	}
	for _, d := range []time.Duration{reconnectMinDelay, reconnectMaxDelay} {
		got := jitter(d)
		low, high := time.Duration(float64(d)*0.85), time.Duration(float64(d)*1.15)
		if got < low || got > high {
			t.Errorf("jitter(%v) = %v, want within ±10%% (allowing rounding)", d, got)
		}
	}
}
