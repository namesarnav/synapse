// Package app holds the bootstrap shared by the api, worker and scheduler binaries.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/migrations"
)

// Process is a running binary's shared state.
type Process struct {
	Cfg  config.Config
	Log  *slog.Logger
	DB   *persistence.DB
	Ctx  context.Context
	stop context.CancelFunc
}

// Boot loads config, connects to Postgres and applies migrations.
func Boot(service string, maxConns int32) (*Process, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := logging.New(service, cfg.LogLevel, os.Stdout, nil)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var db *persistence.DB
	// Postgres may still be starting when the container comes up.
	for attempt := 0; ; attempt++ {
		db, err = persistence.Connect(ctx, cfg.DatabaseURL, maxConns)
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
	return &Process{Cfg: cfg, Log: log, DB: db, Ctx: ctx, stop: stop}, nil
}

// Close releases the process resources.
func (p *Process) Close() {
	p.stop()
	p.DB.Close()
}

// NewRuntime builds the durable runtime from configuration.
func (p *Process) NewRuntime(secrets runtime.SecretProvider) *runtime.Runtime {
	c := p.Cfg
	return runtime.New(&runtime.Runtime{
		DB: p.DB, Log: p.Log, MaxDepth: c.MaxSubWorkflowDepth, MaxDeliveries: c.MaxDeliveries, MaxQueueDepth: c.MaxQueueDepth,
		LeaseDuration: c.LeaseDuration, Secrets: secrets,
	})
}

// ServeOps serves health and metrics endpoints until ctx is cancelled.
func ServeOps(ctx context.Context, addr string, log *slog.Logger, db *persistence.DB, extra ...http.Handler) {
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
	mux.Handle("GET /metrics", promhttp.Handler())
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
