package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/internal/worker"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

type env struct {
	t      *testing.T
	db     *persistence.DB
	url    string
	rt     *runtime.Runtime
	wfs    *wfstore.Store
	ws     string
	userID string
}

func newEnv(t *testing.T, mut ...func(*runtime.Runtime)) *env {
	t.Helper()
	db, url := testutil.NewDBWithURL(t)
	u, w, err := (&auth.Store{DB: db}).Register(context.Background(), "t@example.com", "correct horse battery", "T", "ws")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, url: url, wfs: &wfstore.Store{DB: db}, ws: w.ID, userID: u.ID}
	e.rt = e.newRuntime(mut...)
	return e
}

// newRuntime builds a fresh Runtime over the same database, like a new process.
func (e *env) newRuntime(mut ...func(*runtime.Runtime)) *runtime.Runtime {
	rt := &runtime.Runtime{DB: e.db, LeaseDuration: 600 * time.Millisecond, MaxDeliveries: 3, DefaultNodeTimeout: 5 * time.Second}
	for _, m := range mut {
		m(rt)
	}
	return runtime.New(rt)
}

type nd struct {
	id, typ string
	cfg     any
	opts    func(*workflow.Node)
}

func build(ns []nd, edges ...[3]string) workflow.Graph {
	g := workflow.Graph{}
	for _, n := range ns {
		raw, _ := json.Marshal(n.cfg)
		if n.cfg == nil {
			raw = []byte(`{}`)
		}
		node := workflow.Node{ID: n.id, Type: workflow.NodeType(n.typ), Config: raw}
		if n.opts != nil {
			n.opts(&node)
		}
		g.Nodes = append(g.Nodes, node)
	}
	for i, e := range edges {
		g.Edges = append(g.Edges, workflow.Edge{ID: fmt.Sprintf("e%d", i), Source: e[0], Target: e[1], Branch: e[2]})
	}
	return g
}

func tr(id string) nd { return nd{id: id, typ: "manual_trigger"} }
func tf(id, expr string) nd {
	return nd{id: id, typ: "transform", cfg: map[string]any{"expression": expr}}
}

// publish creates and publishes a workflow, returning its id.
func (e *env) publish(g workflow.Graph) string {
	e.t.Helper()
	w, err := e.wfs.Create(context.Background(), e.ws, e.userID, "wf-"+fmt.Sprint(time.Now().UnixNano()), "", g)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.wfs.Publish(context.Background(), e.ws, w.ID, e.userID, "", func(workflow.Graph) error { return nil }); err != nil {
		e.t.Fatal(err)
	}
	return w.ID
}

func (e *env) start(wfID string, trigger any) *runtime.Execution {
	e.t.Helper()
	res, err := e.rt.Start(context.Background(), runtime.StartParams{WorkspaceID: e.ws, WorkflowID: wfID, Trigger: trigger, CreatedBy: e.userID})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Execution
}

func (e *env) get(id string) *runtime.Execution {
	e.t.Helper()
	ex, err := e.rt.Get(context.Background(), e.ws, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ex
}

// wait polls until the execution reaches a terminal state.
func (e *env) wait(id string) *runtime.Execution {
	e.t.Helper()
	return e.waitFor(id, func(x *runtime.Execution) bool { return x.Status.Terminal() })
}

func (e *env) waitFor(id string, ok func(*runtime.Execution) bool) *runtime.Execution {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ex := e.get(id)
		if ok(ex) {
			return ex
		}
		if time.Now().After(deadline) {
			nodes, _ := e.rt.Nodes(context.Background(), id)
			e.t.Fatalf("timeout waiting for execution %s: status=%s nodes=%+v", id, ex.Status, nodes)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (e *env) nodeState(execID, node string) engine.NodeState {
	e.t.Helper()
	ns, err := e.rt.Nodes(context.Background(), execID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, n := range ns {
		if n.NodeID == node {
			return n.State
		}
	}
	e.t.Fatalf("node %s not found", node)
	return ""
}

// proc is a worker plus scheduler running against the database, like a process.
type proc struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *proc) stop() {
	p.cancel()
	<-p.done
}

type procOpts struct {
	id          string
	concurrency int
	reg         nodes.Registry
	noScheduler bool
	noWorker    bool
	hbEvery     time.Duration
	cfg         func(*worker.Config)
}

func (e *env) spawn(rt *runtime.Runtime, o procOpts) *proc {
	e.t.Helper()
	if o.id == "" {
		o.id = fmt.Sprintf("w-%d", time.Now().UnixNano())
	}
	if o.concurrency == 0 {
		o.concurrency = 4
	}
	if o.reg == nil {
		o.reg = nodes.NewRegistry(nodes.Options{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{cancel: cancel, done: make(chan struct{})}
	cfg := worker.Config{ID: o.id, Concurrency: o.concurrency, PollInterval: 20 * time.Millisecond,
		HeartbeatInterval: o.hbEvery, ShutdownGrace: 2 * time.Second}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	go func() {
		defer close(p.done)
		done := make(chan struct{}, 2)
		if !o.noWorker {
			go func() { _ = worker.New(rt, o.reg, cfg, nil).Run(ctx); done <- struct{}{} }()
		} else {
			done <- struct{}{}
		}
		if !o.noScheduler {
			go func() {
				scheduler.New(rt, scheduler.Config{WakeInterval: 20 * time.Millisecond, ReapInterval: 50 * time.Millisecond,
					SweepInterval: 100 * time.Millisecond, SweepAfter: 200 * time.Millisecond, TimeoutGrace: 100 * time.Millisecond,
					WorkerDeadAfter: 2 * time.Second}, nil).Run(ctx)
				done <- struct{}{}
			}()
		} else {
			done <- struct{}{}
		}
		<-done
		<-done
	}()
	e.t.Cleanup(p.stop)
	return p
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func updateGraph(rev int, g workflow.Graph) wfstore.Update {
	return wfstore.Update{Graph: &g, Revision: rev}
}
