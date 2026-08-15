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

	"github.com/namesarnav/synapse/internal/api"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/cron"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/secrets"
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

	db, err := persistence.Connect(ctx, cfg.DatabaseURL, 20)
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
	rt := runtime.New(&runtime.Runtime{DB: db, Log: log, MaxDepth: cfg.MaxSubWorkflowDepth, MaxDeliveries: cfg.MaxDeliveries,
		MaxQueueDepth: cfg.MaxQueueDepth, LeaseDuration: cfg.LeaseDuration, Secrets: sec})
	if cfg.RunSchedulerInProc {
		tr := &triggers.Store{DB: db, RT: rt, Log: log}
		sch := scheduler.New(rt, scheduler.Config{WakeInterval: cfg.SchedulerTick, ReapInterval: cfg.SchedulerTick,
			SweepInterval: 2 * cfg.SchedulerTick, WorkerDeadAfter: cfg.WorkerDeadAfter, SweepAfter: cfg.SweepAfter,
			ExtraTick: func(ctx context.Context) {
				if _, err := tr.FireDue(ctx, 100); err != nil && ctx.Err() == nil {
					log.Error("fire schedules", "err", err)
				}
			}}, log)
		go sch.Run(ctx)
	}
	srv := api.New(api.Deps{Cfg: cfg, Log: log, DB: db, Runtime: rt, Secrets: sec, Checker: expressions.Checker{Cron: cron.Validate}})
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
