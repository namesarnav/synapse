package main

import (
	"fmt"
	"os"

	"github.com/namesarnav/synapse/internal/app"
	"github.com/namesarnav/synapse/internal/scheduler"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	p, err := app.Boot("scheduler", 0)
	if err != nil {
		return err
	}
	defer p.Close()
	rt := p.NewRuntime(nil)
	s := scheduler.New(rt, scheduler.Config{
		WakeInterval: p.Cfg.SchedulerTick, ReapInterval: p.Cfg.SchedulerTick, SweepInterval: 2 * p.Cfg.SchedulerTick,
		WorkerDeadAfter: p.Cfg.WorkerDeadAfter, SweepAfter: p.Cfg.SweepAfter,
	}, p.Log)
	app.ServeOps(p.Ctx, p.Cfg.WorkerMetricsAddr, p.Log, p.DB)
	s.Run(p.Ctx)
	return nil
}
