package persistence_test

import (
	"context"
	"testing"

	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/migrations"
)

func TestMigrateFromZeroAndIdempotent(t *testing.T) {
	db := testutil.NewDB(t) // already migrated once
	ctx := context.Background()
	ran, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("second run applied %v, want none", ran)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('users','workspaces','sessions')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("tables missing: n=%d err=%v", n, err)
	}
}
