// Package worker claims tasks from the durable queue and runs node executors.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/tracing"
	"github.com/namesarnav/synapse/internal/workflow"
)

// Config tunes a worker.
type Config struct {
	ID                string
	Host              string
	Concurrency       int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	// ShutdownGrace is how long in-flight tasks may finish after a stop request.
	ShutdownGrace time.Duration
	// TypeLimits caps concurrent tasks per node type (0 or missing = unlimited).
	TypeLimits map[string]int
	// Redact scrubs secret values from outputs and errors before they are stored.
	Redact func(workspaceID string, v any) any
}

// Metrics receives worker observations; all fields are optional.
type Metrics struct {
	TaskDone func(nodeType, outcome string, d time.Duration)
	Claimed  func(n int)
	Lost     func()
}

// Worker is one worker process (or in-process worker in tests).
type Worker struct {
	RT      *runtime.Runtime
	Reg     nodes.Registry
	Cfg     Config
	Log     *slog.Logger
	Metrics Metrics

	mu       sync.Mutex
	inflight map[string]*flight
	byType   map[string]int
	wake     chan struct{}
	wg       sync.WaitGroup
}

type flight struct {
	task   runtime.Task
	cancel context.CancelCauseFunc
}

var (
	errLeaseLost = errors.New("lease lost")
	errCancelled = errors.New("execution cancelled")
	errShutdown  = errors.New("worker shutting down")
)

// New returns a worker with defaults filled in.
func New(rt *runtime.Runtime, reg nodes.Registry, cfg Config, log *slog.Logger) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = rt.LeaseDuration / 3
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 20 * time.Second
	}
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Worker{RT: rt, Reg: reg, Cfg: cfg, Log: log.With("worker", cfg.ID),
		inflight: map[string]*flight{}, byType: map[string]int{}, wake: make(chan struct{}, 1)}
}

// Run blocks until ctx is cancelled and in-flight work has drained.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.RT.RegisterWorker(ctx, w.Cfg.ID, w.Cfg.Host, w.Cfg.Concurrency); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.Log.Info("worker started", "concurrency", w.Cfg.Concurrency)
	// runCtx outlives ctx so tasks can finish during the shutdown grace period.
	runCtx, stopRun := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stopRun(nil)

	bg := sync.WaitGroup{}
	loopCtx, stopLoops := context.WithCancel(ctx)
	bg.Add(2)
	go func() { defer bg.Done(); w.listen(loopCtx) }()
	go func() { defer bg.Done(); w.heartbeats(runCtx, loopCtx) }()

	w.claimLoop(loopCtx, runCtx)

	// Drain: stop claiming, let in-flight tasks finish within the grace period.
	_ = w.RT.SetWorkerStatus(context.WithoutCancel(ctx), w.Cfg.ID, "draining")
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		stopRun(nil)
	case <-time.After(w.Cfg.ShutdownGrace):
		w.Log.Warn("shutdown grace elapsed; releasing in-flight tasks")
		stopRun(errShutdown)
		<-done
	}
	stopLoops()
	bg.Wait()
	_ = w.RT.SetWorkerStatus(context.WithoutCancel(ctx), w.Cfg.ID, "stopped")
	w.Log.Info("worker stopped")
	return nil
}

func (w *Worker) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// claimLoop pulls tasks whenever there is spare capacity.
func (w *Worker) claimLoop(ctx, runCtx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-timer.C:
		}
		for ctx.Err() == nil {
			free, exclude := w.capacity()
			if free <= 0 {
				break
			}
			tasks, err := w.RT.Claim(ctx, w.Cfg.ID, free, exclude)
			if err != nil {
				if ctx.Err() == nil {
					w.Log.Error("claim failed", "err", err)
				}
				break
			}
			if len(tasks) == 0 {
				break
			}
			if w.Metrics.Claimed != nil {
				w.Metrics.Claimed(len(tasks))
			}
			for _, t := range tasks {
				// A batch can overshoot a per-type limit; hand the extras back uncounted.
				if !w.admit(t.NodeType) {
					if err := w.RT.Release(context.WithoutCancel(ctx), runtime.TaskRef{ID: t.ID, LeaseToken: t.LeaseToken}); err != nil {
						w.Log.Warn("release over-limit task", "task", t.ID, "err", err)
					}
					continue
				}
				w.start(runCtx, t)
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.Cfg.PollInterval)
	}
}

// capacity returns free slots and the node types that are at their limit.
func (w *Worker) capacity() (int, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var exclude []string
	for typ, lim := range w.Cfg.TypeLimits {
		if lim > 0 && w.byType[typ] >= lim {
			exclude = append(exclude, typ)
		}
	}
	return w.Cfg.Concurrency - len(w.inflight), exclude
}

// admit reports whether another task of this type fits under its limit.
func (w *Worker) admit(nodeType string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	lim := w.Cfg.TypeLimits[nodeType]
	return lim <= 0 || w.byType[nodeType] < lim
}

func (w *Worker) start(runCtx context.Context, t runtime.Task) {
	taskCtx, cancel := context.WithCancelCause(runCtx)
	w.mu.Lock()
	w.inflight[t.ID] = &flight{task: t, cancel: cancel}
	w.byType[t.NodeType]++
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer cancel(nil)
		w.execute(taskCtx, t)
		w.mu.Lock()
		delete(w.inflight, t.ID)
		w.byType[t.NodeType]--
		w.mu.Unlock()
		w.notify()
	}()
}

// execute runs one claimed task end to end.
func (w *Worker) execute(ctx context.Context, t runtime.Task) {
	log := w.Log.With("execution_id", t.ExecutionID, "node_id", t.NodeID, "attempt", t.Attempt, "task_id", t.ID, "worker_id", w.Cfg.ID)
	// Bookkeeping uses a context detached from task cancellation.
	bk := context.WithoutCancel(ctx)
	work, err := w.RT.Begin(bk, w.Cfg.ID, t)
	if err != nil {
		if !errors.Is(err, runtime.ErrLeaseLost) {
			log.Error("begin failed", "err", err)
		}
		return
	}
	log = log.With("workflow_id", work.WorkflowID)
	ctx = tracing.WithTraceparent(ctx, work.Traceparent)
	ctx, span := tracing.Start(ctx, "node "+t.NodeID, attribute.String("synapse.execution_id", t.ExecutionID),
		attribute.String("synapse.workflow_id", work.WorkflowID), attribute.String("synapse.node_id", t.NodeID),
		attribute.String("synapse.node_type", t.NodeType), attribute.Int("synapse.attempt", t.Attempt),
		attribute.String("synapse.worker_id", w.Cfg.ID))
	bk = context.WithoutCancel(ctx)
	start := time.Now()
	out, nerr := w.run(ctx, work, log)
	elapsed := time.Since(start)
	defer func() {
		if nerr != nil {
			span.SetAttributes(attribute.String("synapse.error_code", string(nerr.Code)))
			tracing.End(span, errors.New(nerr.Message))
			return
		}
		span.End()
	}()
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errLeaseLost), errors.Is(cause, errCancelled):
		log.Info("task abandoned", "reason", cause)
		w.observe(t, "abandoned", elapsed)
		return
	case errors.Is(cause, errShutdown):
		if err := w.RT.Release(bk, t.Ref()); err != nil && !errors.Is(err, runtime.ErrLeaseLost) {
			log.Error("release failed", "err", err)
		}
		w.observe(t, "released", elapsed)
		return
	}
	if nerr == nil {
		if w.Cfg.Redact != nil {
			out = w.Cfg.Redact(work.WorkspaceID, out)
		}
		err = w.RT.Complete(bk, t.Ref(), out)
		w.observe(t, "succeeded", elapsed)
	} else {
		if w.Cfg.Redact != nil {
			nerr.Message = fmt.Sprint(w.Cfg.Redact(work.WorkspaceID, nerr.Message))
		}
		err = w.RT.Fail(bk, t.Ref(), nerr)
		w.observe(t, "failed", elapsed)
	}
	if err != nil {
		if errors.Is(err, runtime.ErrLeaseLost) {
			if w.Metrics.Lost != nil {
				w.Metrics.Lost()
			}
			log.Warn("lease lost before result could be recorded")
			return
		}
		log.Error("recording result failed", "err", err)
	}
}

func (w *Worker) observe(t runtime.Task, outcome string, d time.Duration) {
	if w.Metrics.TaskDone != nil {
		w.Metrics.TaskDone(t.NodeType, outcome, d)
	}
}

// run invokes the executor under the node timeout.
func (w *Worker) run(ctx context.Context, work *runtime.Work, log *slog.Logger) (any, *engine.NodeError) {
	ex, ok := w.Reg[work.Node.Type]
	if !ok {
		return nil, &engine.NodeError{Code: engine.CodeInternal, Message: fmt.Sprintf("no executor for node type %q", work.Node.Type)}
	}
	cfg, err := workflow.DecodeConfig(work.Node)
	if err != nil {
		return nil, &engine.NodeError{Code: engine.CodeConfig, Message: err.Error()}
	}
	timeout := work.Task.Timeout
	if timeout <= 0 {
		timeout = w.RT.DefaultNodeTimeout
	}
	tctx, cancel := context.WithTimeoutCause(ctx, timeout, errTimeout)
	defer cancel()
	secret := func(name string) (string, bool) { return w.secret(tctx, work.WorkspaceID, name) }
	var out any
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = &engine.NodeError{Code: engine.CodeInternal, Message: fmt.Sprintf("executor panic: %v", r)}
				out = nil
			}
		}()
		out, err = ex.Execute(tctx, nodes.Input{
			Node: work.Node, Config: cfg, Env: work.Ctx.Env(secret, nil, w.RT.Now), Attempt: work.Task.Attempt,
			IdempotencyKey: work.Task.IdempotencyKey, Log: log,
		})
	}()
	if errors.Is(context.Cause(tctx), errTimeout) {
		return nil, &engine.NodeError{Code: engine.CodeTimeout, Message: fmt.Sprintf("node timed out after %s", timeout), Retryable: true}
	}
	if err != nil {
		var ne *engine.NodeError
		if errors.As(err, &ne) {
			return out, ne
		}
		return nil, &engine.NodeError{Code: engine.CodeInternal, Message: err.Error(), Retryable: true}
	}
	return out, nil
}

var errTimeout = errors.New("node timeout")

// secret loads a workspace secret through the runtime's provider.
func (w *Worker) secret(ctx context.Context, workspaceID, name string) (string, bool) {
	if w.RT.Secrets == nil {
		return "", false
	}
	m, err := w.RT.Secrets.Load(ctx, workspaceID)
	if err != nil {
		return "", false
	}
	v, ok := m[name]
	return v, ok
}

// heartbeats extends leases and applies cancellation and lease-loss signals.
// It keeps beating while draining so leases survive the shutdown grace period.
func (w *Worker) heartbeats(runCtx, loopCtx context.Context) {
	t := time.NewTicker(w.Cfg.HeartbeatInterval)
	defer t.Stop()
	stopped := loopCtx.Done()
	for {
		select {
		case <-t.C:
			w.beat(runCtx)
		case <-runCtx.Done():
			return
		case <-stopped:
			stopped = nil
		}
	}
}

func (w *Worker) inflightCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.inflight)
}

func (w *Worker) beat(ctx context.Context) {
	w.mu.Lock()
	refs := make([]runtime.TaskRef, 0, len(w.inflight))
	for _, f := range w.inflight {
		refs = append(refs, f.task.Ref())
	}
	w.mu.Unlock()
	res, err := w.RT.Heartbeat(context.WithoutCancel(ctx), w.Cfg.ID, refs)
	if err != nil {
		w.Log.Warn("heartbeat failed", "err", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range res.Lost {
		if f := w.inflight[id]; f != nil {
			f.cancel(errLeaseLost)
		}
	}
	for _, id := range res.Cancelled {
		if f := w.inflight[id]; f != nil {
			f.cancel(errCancelled)
		}
	}
}

// listen wakes the claim loop on task notifications.
func (w *Worker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx); err != nil && ctx.Err() == nil {
			w.Log.Warn("listen failed; falling back to polling", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context) error {
	conn, err := w.RT.DB.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+runtime.NotifyChannel); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "UNLISTEN *") }()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		w.notify()
	}
}
