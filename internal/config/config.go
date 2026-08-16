// Package config loads process configuration from environment variables.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the union of settings used by the api, worker and scheduler
// binaries. Each binary reads only what it needs.
type Config struct {
	Env      string // dev | prod
	LogLevel string

	HTTPAddr     string
	DatabaseURL  string
	RedisURL     string // optional: live event fan-out across processes
	MasterKey    []byte // 32 bytes, AES-256-GCM for secrets at rest
	CookieSecure bool
	PublicURL    string

	// Auth
	SessionTTL time.Duration

	// Limits and backpressure
	WebhookMaxBodyBytes int64
	WebhookRatePerSec   float64
	WebhookRateBurst    int
	AuthRatePerMin      int
	APIRatePerSec       float64
	APIRateBurst        int
	MaxQueueDepth       int // webhook/API starts are rejected (503) above this
	MaxSubWorkflowDepth int
	WSClientBuffer      int
	WSMaxConns          int
	AllowedOrigins      []string // extra WebSocket origins (host patterns); same-origin is always allowed

	// Worker
	WorkerID           string
	WorkerCapacity     int
	LeaseDuration      time.Duration
	HeartbeatInterval  time.Duration
	WorkerDeadAfter    time.Duration
	ShutdownTimeout    time.Duration
	MaxDeliveries      int
	HTTPAllowPrivate   bool // SSRF guard off (dev/test only)
	WorkerMetricsAddr  string
	NodeTypeLimits     map[string]int
	PollInterval       time.Duration
	RunSchedulerInProc bool

	// Scheduler
	SchedulerTick     time.Duration
	SweepAfter        time.Duration
	SchedulerInstance string

	// Telemetry
	OTLPEndpoint string
}

func Load() (Config, error) {
	c := Config{
		Env:                 env("SYNAPSE_ENV", "dev"),
		LogLevel:            env("SYNAPSE_LOG_LEVEL", "info"),
		HTTPAddr:            env("SYNAPSE_HTTP_ADDR", ":8080"),
		DatabaseURL:         env("SYNAPSE_DATABASE_URL", "postgres://synapse:synapse@localhost:55432/synapse?sslmode=disable"),
		RedisURL:            env("SYNAPSE_REDIS_URL", ""),
		CookieSecure:        envBool("SYNAPSE_COOKIE_SECURE", false),
		PublicURL:           env("SYNAPSE_PUBLIC_URL", "http://localhost:8080"),
		SessionTTL:          envDur("SYNAPSE_SESSION_TTL", 30*24*time.Hour),
		WebhookMaxBodyBytes: int64(envInt("SYNAPSE_WEBHOOK_MAX_BODY", 1<<20)),
		WebhookRatePerSec:   envFloat("SYNAPSE_WEBHOOK_RATE", 50),
		WebhookRateBurst:    envInt("SYNAPSE_WEBHOOK_BURST", 100),
		AuthRatePerMin:      envInt("SYNAPSE_AUTH_RATE_PER_MIN", 20),
		APIRatePerSec:       envFloat("SYNAPSE_API_RATE", 100),
		APIRateBurst:        envInt("SYNAPSE_API_BURST", 200),
		MaxQueueDepth:       envInt("SYNAPSE_MAX_QUEUE_DEPTH", 100000),
		MaxSubWorkflowDepth: envInt("SYNAPSE_MAX_SUBWORKFLOW_DEPTH", 5),
		WSClientBuffer:      envInt("SYNAPSE_WS_BUFFER", 256),
		WSMaxConns:          envInt("SYNAPSE_WS_MAX_CONNS", 5000),
		AllowedOrigins:      splitList(env("SYNAPSE_ALLOWED_ORIGINS", "")),
		WorkerID:            env("SYNAPSE_WORKER_ID", ""),
		WorkerCapacity:      envInt("SYNAPSE_WORKER_CAPACITY", 16),
		LeaseDuration:       envDur("SYNAPSE_LEASE_DURATION", 30*time.Second),
		HeartbeatInterval:   envDur("SYNAPSE_HEARTBEAT_INTERVAL", 5*time.Second),
		WorkerDeadAfter:     envDur("SYNAPSE_WORKER_DEAD_AFTER", 20*time.Second),
		ShutdownTimeout:     envDur("SYNAPSE_SHUTDOWN_TIMEOUT", 30*time.Second),
		MaxDeliveries:       envInt("SYNAPSE_MAX_DELIVERIES", 5),
		HTTPAllowPrivate:    envBool("SYNAPSE_HTTP_ALLOW_PRIVATE", false),
		WorkerMetricsAddr:   env("SYNAPSE_WORKER_HTTP_ADDR", ":8081"),
		PollInterval:        envDur("SYNAPSE_POLL_INTERVAL", 500*time.Millisecond),
		RunSchedulerInProc:  envBool("SYNAPSE_RUN_SCHEDULER", true),
		SchedulerTick:       envDur("SYNAPSE_SCHEDULER_TICK", time.Second),
		SweepAfter:          envDur("SYNAPSE_SWEEP_AFTER", 15*time.Second),
		OTLPEndpoint:        env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		NodeTypeLimits:      map[string]int{},
	}
	if v := os.Getenv("SYNAPSE_NODE_LIMITS"); v != "" { // e.g. http_request=50,email=10
		for _, kv := range strings.Split(v, ",") {
			p := strings.SplitN(strings.TrimSpace(kv), "=", 2)
			if len(p) != 2 {
				return c, fmt.Errorf("SYNAPSE_NODE_LIMITS: bad entry %q", kv)
			}
			n, err := strconv.Atoi(p[1])
			if err != nil {
				return c, fmt.Errorf("SYNAPSE_NODE_LIMITS: %w", err)
			}
			c.NodeTypeLimits[p[0]] = n
		}
	}
	key, err := parseKey(env("SYNAPSE_MASTER_KEY", ""))
	if err != nil {
		return c, err
	}
	if key == nil {
		if c.Env == "prod" {
			return c, fmt.Errorf("SYNAPSE_MASTER_KEY is required when SYNAPSE_ENV=prod")
		}
		// Deterministic dev key so restarts can still decrypt dev data.
		key = []byte("synapse-dev-master-key-32-bytes!")
	}
	c.MasterKey = key
	return c, nil
}

func parseKey(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("SYNAPSE_MASTER_KEY must be 32 bytes encoded as hex or base64")
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
