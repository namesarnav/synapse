package main

import (
	"fmt"
	"os"

	"github.com/namesarnav/synapse/internal/app"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	p, err := app.Boot("worker", 0)
	if err != nil {
		return err
	}
	defer p.Close()
	cfg := p.Cfg
	host, _ := os.Hostname()
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	bus, _, err := p.NewBus(realtime.NewHub(1), true)
	if err != nil {
		return err
	}
	sec, err := p.NewSecrets()
	if err != nil {
		return err
	}
	rt := p.NewRuntime(sec, bus.Publish)
	reg := nodes.NewRegistry(nodes.Options{HTTP: nodes.HTTPOptions{AllowPrivate: cfg.HTTPAllowPrivate}})
	w := worker.New(rt, reg, worker.Config{
		ID: cfg.WorkerID, Host: host, Concurrency: cfg.WorkerCapacity, PollInterval: cfg.PollInterval,
		HeartbeatInterval: cfg.HeartbeatInterval, ShutdownGrace: cfg.ShutdownTimeout, TypeLimits: cfg.NodeTypeLimits, Redact: sec.RedactFor,
	}, p.Log)
	w.Metrics = p.Metrics.Worker()
	p.ServeOps(cfg.WorkerMetricsAddr)
	return w.Run(p.Ctx)
}
