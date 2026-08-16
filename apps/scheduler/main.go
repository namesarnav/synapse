package main

import (
	"fmt"
	"os"

	"github.com/namesarnav/synapse/internal/app"
	"github.com/namesarnav/synapse/internal/realtime"
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
	bus, _, err := p.NewBus(realtime.NewHub(1), true)
	if err != nil {
		return err
	}
	sec, err := p.NewSecrets()
	if err != nil {
		return err
	}
	s := p.NewScheduler(p.NewRuntime(sec, bus.Publish))
	app.ServeOps(p.Ctx, p.Cfg.WorkerMetricsAddr, p.Log, p.DB)
	s.Run(p.Ctx)
	return nil
}
