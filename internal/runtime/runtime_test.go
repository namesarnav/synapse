package runtime_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
)

func TestBasicRunSucceeds(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{})
	id := e.publish(build([]nd{tr("t"), tf("a", "trigger.x + 1"), tf("b", "nodes.a * 2")},
		[3]string{"t", "a", ""}, [3]string{"a", "b", ""}))
	ex := e.wait(e.start(id, map[string]any{"x": 4.0}).ID)
	if ex.Status != engine.ExecSucceeded || !reflect.DeepEqual(ex.Output, 10.0) {
		t.Fatalf("status=%s output=%v err=%s", ex.Status, ex.Output, ex.Error)
	}
	if e.nodeState(ex.ID, "b") != engine.NodeSucceeded {
		t.Fatal("b not succeeded")
	}
	evs, err := e.rt.Events(context.Background(), ex.ID, 0, 100)
	if err != nil || len(evs) < 6 || evs[0].Type != runtime.EvExecCreated || evs[len(evs)-1].Type != runtime.EvExecSucceeded {
		t.Fatalf("events: %v err=%v", evs, err)
	}
}

func TestStartRequiresPublished(t *testing.T) {
	e := newEnv(t)
	w, err := e.wfs.Create(context.Background(), e.ws, e.userID, "draft", "", build([]nd{tr("t")}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.rt.Start(context.Background(), runtime.StartParams{WorkspaceID: e.ws, WorkflowID: w.ID})
	if !errors.Is(err, runtime.ErrNotPublished) {
		t.Fatalf("err = %v", err)
	}
	_, err = e.rt.Start(context.Background(), runtime.StartParams{WorkspaceID: e.ws, WorkflowID: "not-a-uuid"})
	if !errors.Is(err, runtime.ErrNotPublished) {
		t.Fatalf("bad id err = %v", err)
	}
}

func TestIdempotentStart(t *testing.T) {
	e := newEnv(t)
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	p := runtime.StartParams{WorkspaceID: e.ws, WorkflowID: id, IdempotencyKey: "k1"}
	a, err := e.rt.Start(context.Background(), p)
	if err != nil || a.Duplicate {
		t.Fatalf("first: %v dup=%v", err, a != nil && a.Duplicate)
	}
	b, err := e.rt.Start(context.Background(), p)
	if err != nil || !b.Duplicate || b.Execution.ID != a.Execution.ID {
		t.Fatalf("second: %v %+v", err, b)
	}
	if n := e.count(`SELECT count(*) FROM executions`); n != 1 {
		t.Fatalf("executions = %d", n)
	}
}

func TestConditionBranchesSkipUntaken(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{})
	id := e.publish(build([]nd{tr("t"), {id: "c", typ: "condition", cfg: map[string]any{"expression": "trigger.ok"}}, tf("yes", "1"), tf("no", "2")},
		[3]string{"t", "c", ""}, [3]string{"c", "yes", "true"}, [3]string{"c", "no", "false"}))
	ex := e.wait(e.start(id, map[string]any{"ok": true}).ID)
	if ex.Status != engine.ExecSucceeded || e.nodeState(ex.ID, "no") != engine.NodeSkipped || e.nodeState(ex.ID, "yes") != engine.NodeSucceeded {
		t.Fatalf("status=%s", ex.Status)
	}
}

func TestNodeFailureFailsExecution(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{})
	id := e.publish(build([]nd{tr("t"), tf("a", "1 / 0"), tf("b", "1")}, [3]string{"t", "a", ""}, [3]string{"a", "b", ""}))
	ex := e.wait(e.start(id, nil).ID)
	if ex.Status != engine.ExecFailed || e.nodeState(ex.ID, "b") != engine.NodeCancelled && e.nodeState(ex.ID, "b") != engine.NodeSkipped {
		t.Fatalf("status=%s b=%s", ex.Status, e.nodeState(ex.ID, "b"))
	}
}

func flaky(failures int, calls *atomic.Int32) nodes.Registry {
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		if int(calls.Add(1)) <= failures {
			return nil, &engine.NodeError{Code: engine.CodeNetwork, Message: "flaky", Retryable: true}
		}
		return in.Attempt, nil
	})
	return reg
}

func retryNode(id string, max, delayMS int) nd {
	n := tf(id, "1")
	n.opts = func(n *workflow.Node) {
		n.Retry = &workflow.RetryPolicy{MaxAttempts: max, InitialDelayMS: delayMS, Multiplier: 1}
	}
	return n
}

func TestRetryThenSucceed(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.spawn(e.rt, procOpts{reg: flaky(2, &calls)})
	id := e.publish(build([]nd{tr("t"), retryNode("a", 4, 20)}, [3]string{"t", "a", ""}))
	ex := e.wait(e.start(id, nil).ID)
	if ex.Status != engine.ExecSucceeded || ex.Output != 3.0 {
		t.Fatalf("status=%s output=%v err=%s", ex.Status, ex.Output, ex.Error)
	}
	at, _ := e.rt.Attempts(context.Background(), ex.ID)
	if len(at) != 3 || at[0].Status != "failed" || at[2].Status != "succeeded" {
		t.Fatalf("attempts = %+v", at)
	}
}

func TestRetryExhaustionFails(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.spawn(e.rt, procOpts{reg: flaky(100, &calls)})
	id := e.publish(build([]nd{tr("t"), retryNode("a", 3, 5)}, [3]string{"t", "a", ""}))
	ex := e.wait(e.start(id, nil).ID)
	if ex.Status != engine.ExecFailed || calls.Load() != 3 {
		t.Fatalf("status=%s calls=%d", ex.Status, calls.Load())
	}
}

// A retry scheduled by one process must be picked up by a different process.
func TestRetrySurvivesRestart(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	first := e.spawn(e.rt, procOpts{reg: flaky(1, &calls)})
	id := e.publish(build([]nd{tr("t"), retryNode("a", 3, 700)}, [3]string{"t", "a", ""}))
	ex := e.start(id, nil)
	for calls.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	e.waitFor(ex.ID, func(x *runtime.Execution) bool { return e.nodeState(x.ID, "a") == engine.NodeRetrying })
	first.stop()
	if e.get(ex.ID).Status.Terminal() {
		t.Fatal("execution finished without any process")
	}
	e.spawn(e.newRuntime(), procOpts{reg: flaky(1, &calls)})
	done := e.wait(ex.ID)
	if done.Status != engine.ExecSucceeded {
		t.Fatalf("status=%s err=%s", done.Status, done.Error)
	}
}

func TestDelayIsDurableAcrossRestart(t *testing.T) {
	e := newEnv(t)
	first := e.spawn(e.rt, procOpts{})
	id := e.publish(build([]nd{tr("t"), {id: "d", typ: "delay", cfg: map[string]any{"duration_ms": 600}}, tf("after", "1")},
		[3]string{"t", "d", ""}, [3]string{"d", "after", ""}))
	ex := e.start(id, nil)
	e.waitFor(ex.ID, func(x *runtime.Execution) bool { return x.Status == engine.ExecWaiting })
	first.stop()
	time.Sleep(700 * time.Millisecond) // the wake time passes while nothing runs
	if e.get(ex.ID).Status.Terminal() {
		t.Fatal("finished with no scheduler")
	}
	e.spawn(e.newRuntime(), procOpts{})
	if done := e.wait(ex.ID); done.Status != engine.ExecSucceeded {
		t.Fatalf("status=%s err=%s", done.Status, done.Error)
	}
}

// A worker that dies mid-task (no heartbeat, no completion) must have its task
// redelivered to another worker once the lease expires.
func TestWorkerCrashRedelivers(t *testing.T) {
	e := newEnv(t)
	var started atomic.Int32
	hang := nodes.NewRegistry(nodes.Options{})
	hang[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		started.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	ex := e.start(id, nil)
	// Simulate a crash: claim as a worker, never heartbeat or complete.
	tasks, err := e.rt.Claim(context.Background(), "crashed", 5, nil)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("claim: %v %v", tasks, err)
	}
	if _, err := e.rt.Begin(context.Background(), "crashed", tasks[0]); err != nil {
		t.Fatal(err)
	}
	e.spawn(e.rt, procOpts{})
	done := e.wait(ex.ID)
	if done.Status != engine.ExecSucceeded {
		t.Fatalf("status=%s err=%s", done.Status, done.Error)
	}
	at, _ := e.rt.Attempts(context.Background(), ex.ID)
	if len(at) != 1 || at[0].DeliveryCount != 2 {
		t.Fatalf("want one attempt delivered twice, got %+v", at)
	}
	// The crashed worker's late result must be rejected.
	if err := e.rt.Complete(context.Background(), tasks[0].Ref(), "late"); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatalf("late complete err = %v", err)
	}
	_ = hang
}

func TestFencingRejectsStaleLease(t *testing.T) {
	e := newEnv(t)
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	ex := e.start(id, nil)
	ctx := context.Background()
	a, err := e.rt.Claim(ctx, "A", 1, nil)
	if err != nil || len(a) != 1 {
		t.Fatalf("claim A: %v %v", a, err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tasks SET lease_expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := e.rt.ReapLeases(ctx, nil); err != nil || n != 1 {
		t.Fatalf("reap: %d %v", n, err)
	}
	b, err := e.rt.Claim(ctx, "B", 1, nil)
	if err != nil || len(b) != 1 || b[0].LeaseToken == a[0].LeaseToken {
		t.Fatalf("claim B: %v %v", b, err)
	}
	if _, err := e.rt.Begin(ctx, "A", a[0]); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatalf("stale begin err = %v", err)
	}
	if err := e.rt.Complete(ctx, a[0].Ref(), 1.0); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatalf("stale complete err = %v", err)
	}
	if err := e.rt.Fail(ctx, a[0].Ref(), &engine.NodeError{Code: engine.CodeNodeFailed}); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatalf("stale fail err = %v", err)
	}
	if _, err := e.rt.Begin(ctx, "B", b[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Complete(ctx, b[0].Ref(), 2.0); err != nil {
		t.Fatal(err)
	}
	if done := e.get(ex.ID); done.Status != engine.ExecSucceeded || done.Output != 2.0 {
		t.Fatalf("status=%s output=%v", done.Status, done.Output)
	}
}

func TestDeadLetterAfterMaxDeliveries(t *testing.T) {
	e := newEnv(t, func(rt *runtime.Runtime) { rt.MaxDeliveries = 2; rt.LeaseDuration = 100 * time.Millisecond })
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	ex := e.start(id, nil)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		ts, err := e.rt.Claim(ctx, "crashy", 1, nil)
		if err != nil || len(ts) != 1 {
			t.Fatalf("claim %d: %v %v", i, ts, err)
		}
		time.Sleep(150 * time.Millisecond)
		if _, err := e.rt.ReapLeases(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	done := e.wait(ex.ID)
	if done.Status != engine.ExecFailed {
		t.Fatalf("status=%s", done.Status)
	}
	if n := e.count(`SELECT count(*) FROM tasks WHERE status='dead'`); n != 1 {
		t.Fatalf("dead tasks = %d", n)
	}
}

func TestEachTaskRunsOnceAcrossManyWorkers(t *testing.T) {
	e := newEnv(t)
	var mu = make(chan struct{}, 1)
	runs := map[string]int{}
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		mu <- struct{}{}
		runs[in.IdempotencyKey]++
		<-mu
		time.Sleep(2 * time.Millisecond)
		return 1, nil
	})
	for i := 0; i < 4; i++ {
		e.spawn(e.newRuntime(), procOpts{reg: reg, concurrency: 4, noScheduler: i > 0})
	}
	id := e.publish(build([]nd{tr("t"), tf("a", "1"), tf("b", "1"), tf("c", "1")},
		[3]string{"t", "a", ""}, [3]string{"t", "b", ""}, [3]string{"t", "c", ""}))
	var ids []string
	for i := 0; i < 40; i++ {
		ids = append(ids, e.start(id, nil).ID)
	}
	for _, x := range ids {
		if ex := e.wait(x); ex.Status != engine.ExecSucceeded {
			t.Fatalf("%s: %s %s", x, ex.Status, ex.Error)
		}
	}
	mu <- struct{}{}
	defer func() { <-mu }()
	if len(runs) != 120 {
		t.Fatalf("distinct task runs = %d, want 120", len(runs))
	}
	for k, n := range runs {
		if n != 1 {
			t.Fatalf("%s ran %d times", k, n)
		}
	}
}

func TestNodeTimeout(t *testing.T) {
	e := newEnv(t)
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	e.spawn(e.rt, procOpts{reg: reg})
	n := tf("a", "1")
	n.opts = func(n *workflow.Node) { n.TimeoutMS = 100 }
	id := e.publish(build([]nd{tr("t"), n}, [3]string{"t", "a", ""}))
	ex := e.wait(e.start(id, nil).ID)
	if ex.Status != engine.ExecFailed {
		t.Fatalf("status=%s", ex.Status)
	}
	ns, _ := e.rt.Nodes(context.Background(), ex.ID)
	for _, x := range ns {
		if x.NodeID == "a" && (x.Error == nil || x.Error.Code != engine.CodeTimeout) {
			t.Fatalf("a error = %+v", x.Error)
		}
	}
}

func TestCancelRunningExecution(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 1)
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	e.spawn(e.rt, procOpts{reg: reg, hbEvery: 30 * time.Millisecond})
	id := e.publish(build([]nd{tr("t"), tf("a", "1"), tf("b", "1")}, [3]string{"t", "a", ""}, [3]string{"a", "b", ""}))
	ex := e.start(id, nil)
	<-started
	if err := e.rt.Cancel(context.Background(), ex.ID, "stop it"); err != nil {
		t.Fatal(err)
	}
	done := e.wait(ex.ID)
	if done.Status != engine.ExecCancelled {
		t.Fatalf("status=%s", done.Status)
	}
	if err := e.rt.Cancel(context.Background(), ex.ID, "again"); !errors.Is(err, runtime.ErrTerminal) {
		t.Fatalf("second cancel err = %v", err)
	}
}

func TestCancelQueuedExecutionIsImmediate(t *testing.T) {
	e := newEnv(t)
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	ex := e.start(id, nil)
	if err := e.rt.Cancel(context.Background(), ex.ID, "no"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(ex.ID); got.Status != engine.ExecCancelled {
		t.Fatalf("status=%s", got.Status)
	}
	if n := e.count(`SELECT count(*) FROM tasks WHERE status='queued'`); n != 0 {
		t.Fatalf("queued tasks = %d", n)
	}
}

func TestWorkflowDeadline(t *testing.T) {
	e := newEnv(t, func(rt *runtime.Runtime) { rt.WorkflowTimeout = 300 * time.Millisecond })
	e.spawn(e.rt, procOpts{})
	id := e.publish(build([]nd{tr("t"), {id: "d", typ: "delay", cfg: map[string]any{"duration_ms": 60000}}}, [3]string{"t", "d", ""}))
	ex := e.wait(e.start(id, nil).ID)
	if ex.Status != engine.ExecFailed {
		t.Fatalf("status=%s err=%s", ex.Status, ex.Error)
	}
}

func TestQueueBackpressure(t *testing.T) {
	e := newEnv(t, func(rt *runtime.Runtime) { rt.MaxQueueDepth = 2 })
	id := e.publish(build([]nd{tr("t"), tf("a", "1")}, [3]string{"t", "a", ""}))
	e.start(id, nil)
	e.start(id, nil)
	_, err := e.rt.Start(context.Background(), runtime.StartParams{WorkspaceID: e.ws, WorkflowID: id})
	if !errors.Is(err, runtime.ErrQueueFull) {
		t.Fatalf("err = %v", err)
	}
}
