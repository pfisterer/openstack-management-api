package nodestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pfisterer/cloud-self-service-golib/retry"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// dbStartupPolicy is how long a starting pod waits for its database: up to a minute, pauses from 1 s doubling up to 5 s. Since the NetworkPolicies, kube-router lets a new pod's address through to Postgres only a few seconds after the pod starts, so the first attempts are refused although nothing is wrong (seen on prod, 18.11.2026: "dial error: dial tcp 10.43.x.x:5432" about 6 s after the start).
var dbStartupPolicy = retry.Policy{Total: time.Minute, Initial: time.Second, Max: 5 * time.Second, Jitter: 0.1}

// openDBWithRetry opens the database through dialector, tried again under p while the attempt fails for a reason that waiting can fix (see retryableConnectError). Each failed attempt that will be tried again is logged at warn level, when log is set; an attempt that cannot succeed later (wrong password, missing database, malformed DSN, a sqlite file that cannot be opened) ends it at once. GORM logs a failed gorm.Open as an error by itself ("failed to initialize database"), which would raise the AppLogError alert for every refused attempt, so the attempts run with GORM's logger discarded and runtime takes over once the connection is there; the caller logs the final failure. runtime nil means GORM's default logger.
func openDBWithRetry(ctx context.Context, p retry.Policy, log *zap.SugaredLogger, dialector gorm.Dialector, runtime gormlogger.Interface) (*gorm.DB, error) {
	if runtime == nil {
		runtime = gormlogger.Default
	}
	p.OnRetry = func(attempt int, err error, wait time.Duration) {
		if log != nil {
			log.Warnw("database not reachable yet, trying again", "attempt", attempt, "retry_in", wait.Round(time.Millisecond).String(), "error", err)
		}
	}
	var db *gorm.DB
	attempts := 0
	err := retry.Do(ctx, p, func(context.Context) error {
		attempts++
		var err error
		db, err = gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Discard})
		if err != nil && !retryableConnectError(err) {
			return retry.Permanent(err)
		}
		return err
	})
	if err != nil {
		if attempts > 1 {
			return nil, fmt.Errorf("gave up after %d attempts: %w", attempts, err)
		}
		return nil, err
	}
	db.Logger = runtime
	return db, nil
}

// retryableConnectError tells whether a failed connection attempt may succeed later: the database was not reachable (refused, reset, timed out, name not resolved) or said it cannot take the connection yet (starting up, too many connections). An answer from the server that rejects the login (wrong password, unknown role or database, no pg_hba entry) and a DSN that does not parse are final.
func retryableConnectError(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		// 57P03 cannot_connect_now (starting up or shutting down), 53300 too_many_connections.
		return pgErr.Code == "57P03" || pgErr.Code == "53300"
	}
	if _, ok := errors.AsType[*pgconn.ParseConfigError](err); ok {
		return false
	}
	if pgconn.Timeout(err) {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}
