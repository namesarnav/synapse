package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/namesarnav/synapse/internal/api"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/cron"
	"github.com/namesarnav/synapse/internal/expressions"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New("api", cfg.LogLevel, os.Stdout, nil)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	traceDown, err := tracing.Init(ctx, "synapse-api", cfg.OTLPEndpoint)
	if err != nil {
		log.Warn("tracing disabled", "err", err)
		traceDown = func(context.Context) error { return nil }
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = traceDown(c)
	}()

	db, err := persistence.ConnectTraced(ctx, cfg.DatabaseURL, 20, tracing.PGX{})
	if err != nil {
		return err
	}
	defer db.Close()
	ran, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "count", len(ran))

	sec, err := secrets.New(db, cfg.MasterKey)
	if err != nil {
		return err
	}
	metrics := telemetry.New()
	metrics.RegisterDB(db)
	hub := realtime.NewHub(cfg.WSClientBuffer)
	hub.Metrics = metrics.Hub()
	var rc *redis.Client
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("parse SYNAPSE_REDIS_URL: %w", err)
		}
		rc = redis.NewClient(opt)
		defer rc.Close()
	}
	bus := realtime.NewBus(hub, rc, log)
	go bus.Run(ctx)
	rt := runtime.New(&runtime.Runtime{DB: db, Log: log, MaxDepth: cfg.MaxSubWorkflowDepth, MaxDeliveries: cfg.MaxDeliveries,
		MaxQueueDepth: cfg.MaxQueueDepth, LeaseDuration: cfg.LeaseDuration, Secrets: sec,
		OnEvents: func(evs []runtime.Event) {
			metrics.ObserveEvents(evs)
			bus.Publish(evs)
		}})
	if cfg.RunSchedulerInProc {
		tr := &triggers.Store{DB: db, RT: rt, Log: log}
		sch := scheduler.New(rt, scheduler.Config{WakeInterval: cfg.SchedulerTick, ReapInterval: cfg.SchedulerTick,
			SweepInterval: 2 * cfg.SchedulerTick, WorkerDeadAfter: cfg.WorkerDeadAfter, SweepAfter: cfg.SweepAfter,
			ExtraTick: func(ctx context.Context) {
				if _, err := tr.FireDue(ctx, 100); err != nil && ctx.Err() == nil {
					log.Error("fire schedules", "err", err)
				}
			}}, log)
		sch.Metrics = metrics.Scheduler()
		go sch.Run(ctx)
	}
	srv := api.New(api.Deps{Cfg: cfg, Log: log, DB: db, Runtime: rt, Hub: hub, Secrets: sec, Metrics: metrics, Checker: expressions.Checker{Cron: cron.Validate}})
	hs := &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return hs.Shutdown(sctx)
}
