package persistence

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrationLockID serialises concurrent migrators (several API replicas
// starting at once).
const migrationLockID = 727274

// Migrate applies every not-yet-applied migration from fsys in version order.
// Migration files are named NNNN_description.sql. Each file runs in its own
// transaction together with its schema_migrations row.
func (d *DB) Migrate(ctx context.Context, fsys fs.FS) ([]string, error) {
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return nil, fmt.Errorf("migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	applied := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	var ran []string
	for _, name := range entries {
		ver, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return ran, fmt.Errorf("migration %q: file name must start with a version number", name)
		}
		if applied[ver] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return ran, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ran, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return ran, fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version, name) VALUES ($1,$2)`, ver, name); err != nil {
			_ = tx.Rollback(ctx)
			return ran, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, err
		}
		ran = append(ran, name)
	}
	return ran, nil
}
