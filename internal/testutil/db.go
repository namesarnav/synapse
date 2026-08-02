// Package testutil provides helpers shared by integration tests.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/migrations"
)

const defaultAdminURL = "postgres://synapse:synapse@localhost:55432/postgres?sslmode=disable"

// AdminURL returns the connection string used to create throwaway databases.
func AdminURL() string {
	if v := os.Getenv("SYNAPSE_TEST_ADMIN_URL"); v != "" {
		return v
	}
	return defaultAdminURL
}

// NewDB creates an isolated, fully migrated database for one test and drops it
// on cleanup. The test is skipped when PostgreSQL is unreachable unless
// SYNAPSE_REQUIRE_DB=1 (used by `make integration` and CI).
func NewDB(t testing.TB) *persistence.DB {
	t.Helper()
	db, _ := NewDBWithURL(t)
	return db
}

// NewDBWithURL is NewDB but also returns the database URL, for tests that
// spawn additional connections (multiple worker "processes").
func NewDBWithURL(t testing.TB) (*persistence.DB, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		if os.Getenv("SYNAPSE_REQUIRE_DB") == "1" {
			t.Fatalf("postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable (set SYNAPSE_TEST_ADMIN_URL or run `make dev-infra`): %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "synapse_t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	u, _ := url.Parse(AdminURL())
	u.Path = "/" + name
	dbURL := u.String()

	db, err := persistence.Connect(ctx, dbURL, 40)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	if _, err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close(cctx)
	})
	return db, dbURL
}
