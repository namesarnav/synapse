// Package app holds the bootstrap shared by the api, worker and scheduler binaries.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/secrets"
	"github.com/namesarnav/synapse/internal/telemetry"
	"github.com/namesarnav/synapse/internal/tracing"
	"github.com/namesarnav/synapse/internal/triggers"
	"github.com/namesarnav/synapse/migrations"
)

// Process is a running binary's shared state.
type Process struct {
	Cfg config.Config
	Log *slog.Logger
	DB  *persistence.DB
	Ctx context.Context
	// Metrics is the process-wide Prometheus registry.
	Metrics   *telemetry.Metrics
	stop      context.CancelFunc
	traceDown func(context.Context) error
}

// Boot loads config, connects to Postgres and applies migrations.
func Boot(service string, maxConns int32) (*Process, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := logging.New(service, cfg.LogLevel, os.Stdout, nil)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	traceDown, terr := tracing.Init(ctx, "synapse-"+service, cfg.OTLPEndpoint)
	if terr != nil {
		log.Warn("tracing disabled", "err", terr)
		traceDown = func(context.Context) error { return nil }
	}
	var db *persistence.DB
	// Postgres may still be starting when the container comes up.
	for attempt := 0; ; attempt++ {
		db, err = persistence.ConnectTraced(ctx, cfg.DatabaseURL, maxConns, tracing.PGX{})
		if err == nil {
			break
		}
		if attempt >= 30 || ctx.Err() != nil {
			stop()
			return nil, err
		}
		log.Warn("waiting for postgres", "err", err)
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	ran, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		stop()
		db.Close()
		return nil, err
	}
	if len(ran) > 0 {
		log.Info("migrations applied", "count", len(ran))
	}
	m := telemetry.New()
	m.RegisterDB(db)
	return &Process{Cfg: cfg, Log: log, DB: db, Ctx: ctx, Metrics: m, stop: stop, traceDown: traceDown}, nil
}

// Close releases the process resources and flushes pending spans.
func (p *Process) Close() {
	p.stop()
	p.DB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.traceDown(ctx)
}

// NewBus connects live event fan-out to Redis when configured. Processes that
// only emit events (workers, scheduler) pass publishOnly.
func (p *Process) NewBus(hub *realtime.Hub, publishOnly bool) (*realtime.Bus, *redis.Client, error) {
	var rc *redis.Client
	if p.Cfg.RedisURL != "" {
		opt, err := redis.ParseURL(p.Cfg.RedisURL)
		if err != nil {
			return nil, nil, fmt.Errorf("parse SYNAPSE_REDIS_URL: %w", err)
		}
		rc = redis.NewClient(opt)
	}
	hub.Metrics = p.Metrics.Hub()
	b := realtime.NewBus(hub, rc, p.Log)
	b.PublishOnly = publishOnly
	go b.Run(p.Ctx)
	return b, rc, nil
}

// NewSecrets builds the encrypted secret store from the master key.
func (p *Process) NewSecrets() (*secrets.Store, error) {
	return secrets.New(p.DB, p.Cfg.MasterKey)
}

// NewScheduler builds a scheduler that also fires cron schedules.
func (p *Process) NewScheduler(rt *runtime.Runtime) *scheduler.Scheduler {
	c := p.Cfg
	tr := &triggers.Store{DB: p.DB, RT: rt, Log: p.Log}
	s := scheduler.New(rt, scheduler.Config{
		WakeInterval: c.SchedulerTick, ReapInterval: c.SchedulerTick, SweepInterval: 2 * c.SchedulerTick,
		WorkerDeadAfter: c.WorkerDeadAfter, SweepAfter: c.SweepAfter,
		ExtraTick: func(ctx context.Context) {
			if _, err := tr.FireDue(ctx, 100); err != nil && ctx.Err() == nil {
				p.Log.Error("fire schedules", "err", err)
			}
		},
	}, p.Log)
	s.Metrics = p.Metrics.Scheduler()
	return s
}

// NewRuntime builds the durable runtime from configuration.
func (p *Process) NewRuntime(secrets runtime.SecretProvider, onEvents func([]runtime.Event)) *runtime.Runtime {
	c := p.Cfg
	return runtime.New(&runtime.Runtime{
		DB: p.DB, Log: p.Log, MaxDepth: c.MaxSubWorkflowDepth, MaxDeliveries: c.MaxDeliveries, MaxQueueDepth: c.MaxQueueDepth,
		LeaseDuration: c.LeaseDuration, Secrets: secrets, OnEvents: func(evs []runtime.Event) {
			p.Metrics.ObserveEvents(evs)
			if onEvents != nil {
				onEvents(evs)
			}
		},
	})
}

// ServeOps serves health and metrics endpoints until the process stops.
func (p *Process) ServeOps(addr string) {
	ctx, log, db := p.Ctx, p.Log, p.DB
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(c); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", p.Metrics.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("ops server failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
}
