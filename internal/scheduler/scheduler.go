// Package scheduler runs the periodic maintenance loops of the engine: waking
// delayed nodes, expiring deadlines, reaping dead leases and sweeping
// unacknowledged child completions. Every operation is idempotent and uses
// row-level locks, so any number of scheduler instances may run concurrently.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/namesarnav/synapse/internal/runtime"
)

// Config sets loop intervals. Zero values take defaults.
type Config struct {
	WakeInterval      time.Duration // delayed nodes and deadlines
	ReapInterval      time.Duration // leases, dead workers, node timeouts
	SweepInterval     time.Duration // child completion sweeper
	WorkerDeadAfter   time.Duration
	TimeoutGrace      time.Duration
	SweepAfter        time.Duration
	BatchSize         int
	ExtraTick         func(ctx context.Context) // optional hook (cron checker)
	ExtraTickInterval time.Duration
}

// Metrics receives counts from each loop; all fields are optional.
type Metrics struct {
	Woken, Expired, Requeued, TimedOut, Swept func(n int)
}

// Scheduler owns the maintenance loops.
type Scheduler struct {
	RT      *runtime.Runtime
	Cfg     Config
	Log     *slog.Logger
	Metrics Metrics
}

// New returns a Scheduler with defaults applied.
func New(rt *runtime.Runtime, cfg Config, log *slog.Logger) *Scheduler {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.WakeInterval, 500*time.Millisecond)
	def(&cfg.ReapInterval, time.Second)
	def(&cfg.SweepInterval, 2*time.Second)
	def(&cfg.WorkerDeadAfter, 3*rt.LeaseDuration/2)
	def(&cfg.TimeoutGrace, 5*time.Second)
	def(&cfg.SweepAfter, 2*time.Second)
	def(&cfg.ExtraTickInterval, time.Second)
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{RT: rt, Cfg: cfg, Log: log.With("component", "scheduler")}
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loop := func(every time.Duration, name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := fn(ctx); err != nil && ctx.Err() == nil {
						s.Log.Error("scheduler loop failed", "loop", name, "err", err)
					}
				}
			}
		}()
	}
	loop(s.Cfg.WakeInterval, "wake", s.wake)
	loop(s.Cfg.ReapInterval, "reap", s.reap)
	loop(s.Cfg.SweepInterval, "sweep", s.sweep)
	if s.Cfg.ExtraTick != nil {
		loop(s.Cfg.ExtraTickInterval, "extra", func(ctx context.Context) error { s.Cfg.ExtraTick(ctx); return nil })
	}
	wg.Wait()
}

// Tick runs every loop body once; tests and one-shot tools use it.
func (s *Scheduler) Tick(ctx context.Context) error {
	for _, fn := range []func(context.Context) error{s.wake, s.reap, s.sweep} {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) report(f func(int), n int) {
	if f != nil && n > 0 {
		f(n)
	}
}

func (s *Scheduler) wake(ctx context.Context) error {
	n, err := s.RT.WakeDue(ctx, s.Cfg.BatchSize)
	s.report(s.Metrics.Woken, n)
	if err != nil {
		return err
	}
	n, err = s.RT.ExpireDeadlines(ctx, s.Cfg.BatchSize)
	s.report(s.Metrics.Expired, n)
	return err
}

func (s *Scheduler) reap(ctx context.Context) error {
	dead, err := s.RT.DeadWorkers(ctx, s.Cfg.WorkerDeadAfter)
	if err != nil {
		return err
	}
	if len(dead) > 0 {
		s.Log.Warn("workers presumed dead", "workers", dead)
	}
	n, err := s.RT.ReapLeases(ctx, dead)
	s.report(s.Metrics.Requeued, n)
	if err != nil {
		return err
	}
	n, err = s.RT.TimeoutTasks(ctx, s.Cfg.TimeoutGrace)
	s.report(s.Metrics.TimedOut, n)
	return err
}

func (s *Scheduler) sweep(ctx context.Context) error {
	n, err := s.RT.SweepChildren(ctx, s.Cfg.SweepAfter, s.Cfg.BatchSize)
	s.report(s.Metrics.Swept, n)
	return err
}
