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
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
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

	rt := runtime.New(&runtime.Runtime{DB: db, Log: log, MaxDepth: cfg.MaxSubWorkflowDepth, MaxDeliveries: cfg.MaxDeliveries,
		MaxQueueDepth: cfg.MaxQueueDepth, LeaseDuration: cfg.LeaseDuration})
	if cfg.RunSchedulerInProc {
		sch := scheduler.New(rt, scheduler.Config{WakeInterval: cfg.SchedulerTick, ReapInterval: cfg.SchedulerTick,
			SweepInterval: 2 * cfg.SchedulerTick, WorkerDeadAfter: cfg.WorkerDeadAfter, SweepAfter: cfg.SweepAfter}, log)
		go sch.Run(ctx)
	}
	srv := api.New(api.Deps{Cfg: cfg, Log: log, DB: db, Runtime: rt, Checker: expressions.Checker{}})
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
