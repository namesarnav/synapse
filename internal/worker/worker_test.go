package worker

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/secrets"
	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

type env struct {
	t    *testing.T
	db   *persistence.DB
	rt   *runtime.Runtime
	sec  *secrets.Store
	ws   string
	user string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testutil.NewDB(t)
	u, ws, err := (&auth.Store{DB: db}).Register(context.Background(), "w@example.com", "correct-horse-battery", "W", "w")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(&runtime.Runtime{DB: db, LeaseDuration: 2 * time.Second, Secrets: sec})
	e := &env{t: t, db: db, rt: rt, sec: sec, ws: ws.ID, user: u.ID}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		scheduler.New(rt, scheduler.Config{WakeInterval: 20 * time.Millisecond, ReapInterval: 50 * time.Millisecond}, nil).Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

func (e *env) run(nodesList []workflow.Node, edges []workflow.Edge) string {
	e.t.Helper()
	ctx := context.Background()
	st := &wfstore.Store{DB: e.db}
	w, err := st.Create(ctx, e.ws, e.user, "wf", "", workflow.Graph{Nodes: nodesList, Edges: edges})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := st.Publish(ctx, e.ws, w.ID, e.user, "", func(workflow.Graph) error { return nil }); err != nil {
		e.t.Fatal(err)
	}
	res, err := e.rt.Start(ctx, runtime.StartParams{WorkspaceID: e.ws, WorkflowID: w.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Execution.ID
}

func (e *env) wait(id string) *runtime.Execution {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ex, err := e.rt.Get(context.Background(), e.ws, id)
		if err != nil {
			e.t.Fatal(err)
		}
		if ex.Status.Terminal() {
			return ex
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("execution stuck in %s", ex.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) spawn(reg nodes.Registry, cfg Config) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cfg.PollInterval = 20 * time.Millisecond
	w := New(e.rt, reg, cfg, nil)
	go func() { _ = w.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	e.t.Cleanup(stop)
	return stop
}

func node(id string, typ workflow.NodeType, cfg string) workflow.Node {
	n := workflow.Node{ID: id, Type: typ}
	if cfg != "" {
		n.Config = json.RawMessage(cfg)
	}
	return n
}

func edge(id, from, to string) workflow.Edge { return workflow.Edge{ID: id, Source: from, Target: to} }

func TestSecretsResolveAndAreRedacted(t *testing.T) {
	e := newEnv(t)
	if err := e.sec.Set(context.Background(), e.ws, "API_TOKEN", "tok-abcdef-123", ""); err != nil {
		t.Fatal(err)
	}
	id := e.run([]workflow.Node{
		node("t", workflow.TypeManualTrigger, ""),
		node("x", workflow.TypeTransform, `{"output":{"auth":"Bearer {{ secrets.API_TOKEN }}","n":1}}`),
	}, []workflow.Edge{edge("e1", "t", "x")})
	e.spawn(nodes.NewRegistry(nodes.Options{}), Config{Concurrency: 2, Redact: e.sec.RedactFor})
	ex := e.wait(id)
	if ex.Status != engine.ExecSucceeded {
		t.Fatalf("status = %s (%s)", ex.Status, ex.Error)
	}
	nx, _ := e.rt.Nodes(context.Background(), id)
	for _, n := range nx {
		if n.NodeID == "x" {
			b, _ := json.Marshal(n.Output)
			if string(b) != `{"auth":"Bearer ***","n":1}` {
				t.Fatalf("output = %s", b)
			}
		}
	}
}

func TestTypeLimitCapsConcurrency(t *testing.T) {
	e := newEnv(t)
	var cur, peak atomic.Int32
	reg := nodes.Registry{workflow.TypeLog: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		cur.Add(-1)
		return map[string]any{"ok": true}, nil
	})}
	nl := []workflow.Node{node("t", workflow.TypeManualTrigger, "")}
	var edges []workflow.Edge
	for _, id := range []string{"a", "b", "c", "d", "f"} {
		nl = append(nl, node(id, workflow.TypeLog, `{"message":"x"}`))
		edges = append(edges, edge("e"+id, "t", id))
	}
	id := e.run(nl, edges)
	e.spawn(reg, Config{Concurrency: 8, TypeLimits: map[string]int{"log": 2}})
	if ex := e.wait(id); ex.Status != engine.ExecSucceeded {
		t.Fatalf("status = %s", ex.Status)
	}
	if p := peak.Load(); p > 2 || p < 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

func TestShutdownGraceReleasesInFlightTask(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	slow := nodes.Registry{workflow.TypeLog: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done() // never finishes on its own
		return nil, ctx.Err()
	})}
	id := e.run([]workflow.Node{node("t", workflow.TypeManualTrigger, ""), node("a", workflow.TypeLog, `{"message":"x"}`)},
		[]workflow.Edge{edge("e1", "t", "a")})
	stop := e.spawn(slow, Config{ID: "w-slow", Concurrency: 1, ShutdownGrace: 200 * time.Millisecond})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("task never started")
	}
	stop() // grace elapses, task is released rather than failed

	fast := nodes.Registry{workflow.TypeLog: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		return map[string]any{"ok": true}, nil
	})}
	e.spawn(fast, Config{ID: "w-fast", Concurrency: 1})
	ex := e.wait(id)
	if ex.Status != engine.ExecSucceeded {
		t.Fatalf("status = %s (%s)", ex.Status, ex.Error)
	}
	var attempts int
	_ = e.db.Pool.QueryRow(context.Background(), `SELECT max(attempt) FROM node_executions WHERE execution_id=$1 AND node_id='a'`, id).Scan(&attempts)
	if attempts != 1 {
		t.Fatalf("shutdown consumed a retry attempt: attempt=%d", attempts)
	}
}

func TestPanicIsContained(t *testing.T) {
	e := newEnv(t)
	reg := nodes.Registry{workflow.TypeLog: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		panic("boom")
	})}
	id := e.run([]workflow.Node{node("t", workflow.TypeManualTrigger, ""), node("a", workflow.TypeLog, `{"message":"x"}`)},
		[]workflow.Edge{edge("e1", "t", "a")})
	e.spawn(reg, Config{Concurrency: 1})
	ex := e.wait(id)
	if ex.Status != engine.ExecFailed {
		t.Fatalf("status = %s, want failed", ex.Status)
	}
}
