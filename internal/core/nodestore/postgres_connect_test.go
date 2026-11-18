package nodestore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pfisterer/cloud-self-service-golib/retry"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// flakyDialector fails Initialize with the next error of errs, one per call, and succeeds once they are used up (or, with forever, keeps failing with the last); it stands in for a database that refuses the first connections.
type flakyDialector struct {
	gorm.Dialector // unused methods; gorm.Open calls only Name and Initialize
	mu             sync.Mutex
	errs           []error
	forever        bool
	calls          int
}

func (d *flakyDialector) Name() string { return "flaky" }

func (d *flakyDialector) Initialize(db *gorm.DB) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if len(d.errs) > 0 {
		err := d.errs[0]
		if len(d.errs) > 1 || !d.forever {
			d.errs = d.errs[1:]
		}
		return err
	}
	db.ConnPool = stubConnPool{}
	return nil
}

type stubConnPool struct{ gorm.ConnPool }

// recordingLogger counts what GORM would have logged.
type recordingLogger struct {
	gormlogger.Interface
	mu     sync.Mutex
	errors []string
}

func (l *recordingLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *recordingLogger) Error(_ context.Context, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf(msg, args...))
}

// refused is what a dial looks like that the NetworkPolicy has not let through yet.
func refused() error {
	return fmt.Errorf("failed to connect to `user=test database=test`: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
}

var fastPolicy = retry.Policy{Total: 5 * time.Second, Initial: time.Millisecond, Max: 5 * time.Millisecond}

func observedLogger() (*zap.SugaredLogger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core).Sugar(), logs
}

func TestOpenDBWithRetry_RefusedThenConnects(t *testing.T) {
	// GORM's default logger is what prints "[error] failed to initialize database"; swap it for a recorder to see that the attempts do not reach it.
	saved := gormlogger.Default
	gl := &recordingLogger{}
	gormlogger.Default = gl
	defer func() { gormlogger.Default = saved }()

	log, logs := observedLogger()
	d := &flakyDialector{errs: []error{refused(), refused(), refused()}}
	db, err := openDBWithRetry(context.Background(), fastPolicy, log, d, nil)
	if err != nil {
		t.Fatalf("openDBWithRetry: %v", err)
	}
	if d.calls != 4 {
		t.Errorf("attempts = %d, want 4", d.calls)
	}
	if db.Logger != gl {
		t.Errorf("GORM logger after connecting = %T, want the runtime logger", db.Logger)
	}
	if len(gl.errors) != 0 {
		t.Errorf("GORM logged errors during the attempts: %q", gl.errors)
	}
	entries := logs.All()
	if len(entries) != 3 {
		t.Fatalf("log lines = %d, want 3 (one per retried attempt): %v", len(entries), entries)
	}
	for i, e := range entries {
		if e.Level != zapcore.WarnLevel {
			t.Errorf("line %d at %v, want warn", i, e.Level)
		}
		if got := e.ContextMap()["attempt"]; got != int64(i+1) {
			t.Errorf("line %d attempt = %v, want %d", i, got, i+1)
		}
		if !strings.Contains(fmt.Sprint(e.ContextMap()["error"]), "connection refused") {
			t.Errorf("line %d error = %v, want the dial error", i, e.ContextMap()["error"])
		}
	}
}

func TestOpenDBWithRetry_GivesUpAtDeadline(t *testing.T) {
	log, logs := observedLogger()
	d := &flakyDialector{errs: []error{refused()}, forever: true}
	start := time.Now()
	_, err := openDBWithRetry(context.Background(), retry.Policy{Total: 200 * time.Millisecond, Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond}, log, d, &recordingLogger{})
	if err == nil {
		t.Fatal("want an error once the time is up")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want about the 200 ms of the policy", took)
	}
	if d.calls < 3 {
		t.Errorf("attempts = %d, want several", d.calls)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("after %d attempts", d.calls)) || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("error = %v, want the attempt count and the last dial error", err)
	}
	for _, e := range logs.All() {
		if e.Level > zapcore.WarnLevel {
			t.Errorf("logged at %v: %s; the final failure is the caller's to log", e.Level, e.Message)
		}
	}
	if n := logs.Len(); n != d.calls-1 {
		t.Errorf("warn lines = %d, want %d (every attempt but the last)", n, d.calls-1)
	}
}

func TestOpenDBWithRetry_AuthErrorIsFinal(t *testing.T) {
	log, logs := observedLogger()
	authErr := fmt.Errorf("failed to connect to `user=test database=test`: %w", &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed for user \"test\""})
	d := &flakyDialector{errs: []error{authErr, refused()}}
	_, err := openDBWithRetry(context.Background(), fastPolicy, log, d, &recordingLogger{})
	if err == nil || !strings.Contains(err.Error(), "password authentication failed") {
		t.Fatalf("error = %v, want the authentication error", err)
	}
	if d.calls != 1 {
		t.Errorf("attempts = %d, want 1", d.calls)
	}
	if logs.Len() != 0 {
		t.Errorf("logged %d lines, want none", logs.Len())
	}
}

func TestOpenDBWithRetry_NilLog(t *testing.T) {
	d := &flakyDialector{errs: []error{refused()}}
	if _, err := openDBWithRetry(context.Background(), fastPolicy, nil, d, &recordingLogger{}); err != nil {
		t.Fatalf("openDBWithRetry: %v", err)
	}
}

func TestRetryableConnectError(t *testing.T) {
	// A real refused connection, as pgx reports it: nothing listens on the port of a listener just closed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, realRefused := pgconn.Connect(ctx, fmt.Sprintf("postgres://u:p@127.0.0.1:%d/db?sslmode=disable&connect_timeout=2", addr.Port))
	_, parseErr := pgconn.ParseConfig("postgres://u:p@host:notaport/db")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"pgx connection refused", realRefused, true},
		{"dial refused", refused(), true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutErr{}}, true},
		{"dns", &net.DNSError{Err: "no such host", Name: "postgres", IsNotFound: true}, true},
		{"starting up", &pgconn.PgError{Code: "57P03"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"wrong password", &pgconn.PgError{Code: "28P01"}, false},
		{"no pg_hba entry", &pgconn.PgError{Code: "28000"}, false},
		{"missing database", &pgconn.PgError{Code: "3D000"}, false},
		{"bad dsn", parseErr, false},
		{"other", errors.New("unable to open database file"), false},
	}
	for _, c := range cases {
		if c.err == nil {
			t.Fatalf("%s: no error to classify", c.name)
		}
		if got := retryableConnectError(c.err); got != c.want {
			t.Errorf("%s (%v): retryable = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
