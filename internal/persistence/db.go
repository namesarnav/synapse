// Package persistence owns the PostgreSQL connection pool, the transaction
// helper and the schema migration runner. Domain-specific SQL lives in the
// store subpackages next to the domain they serve, never in HTTP handlers.
package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgx pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens a pool and verifies connectivity.
func Connect(ctx context.Context, url string, maxConns int32) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() { d.Pool.Close() }

func (d *DB) Ping(ctx context.Context) error { return d.Pool.Ping(ctx) }

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx so store functions can
// run inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Tx is a transaction with after-commit hooks. Hooks are how live events are
// published only once the state they describe is durable.
type Tx struct {
	pgx.Tx
	hooks []func()
}

// AfterCommit registers fn to run after a successful commit.
func (t *Tx) AfterCommit(fn func()) { t.hooks = append(t.hooks, fn) }

const maxTxRetries = 5

// InTx runs fn in a transaction, committing on nil and rolling back on error.
// Serialization failures and deadlocks are retried with the whole function
// re-run, so fn must be safe to re-execute.
func (d *DB) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	var err error
	for attempt := 0; attempt < maxTxRetries; attempt++ {
		err = d.inTxOnce(ctx, fn)
		if err == nil || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
	return err
}

func (d *DB) inTxOnce(ctx context.Context, fn func(tx *Tx) error) error {
	ptx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	tx := &Tx{Tx: ptx}
	if err := fn(tx); err != nil {
		_ = ptx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return err
	}
	for _, h := range tx.hooks {
		h()
	}
	return nil
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}
	return false
}

// IsUniqueViolation reports a unique-constraint failure, optionally for a
// specific constraint name (empty matches any).
func IsUniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return constraint == "" || pe.ConstraintName == constraint
	}
	return false
}

// ErrNotFound is returned by stores when a row does not exist (or is not
// visible to the caller's workspace).
var ErrNotFound = errors.New("not found")

// NotFound maps pgx.ErrNoRows to ErrNotFound.
func NotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
