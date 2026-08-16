package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultRedisURL = "redis://localhost:56379/15"

// NewRedis returns a client for the test Redis, skipping the test when it is
// unreachable unless SYNAPSE_REQUIRE_DB=1.
func NewRedis(t testing.TB) *redis.Client {
	t.Helper()
	url := os.Getenv("SYNAPSE_TEST_REDIS_URL")
	if url == "" {
		url = defaultRedisURL
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		if os.Getenv("SYNAPSE_REQUIRE_DB") == "1" {
			t.Fatalf("redis unavailable: %v", err)
		}
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
