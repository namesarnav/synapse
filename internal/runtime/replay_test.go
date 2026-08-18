package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
)

// countingRegistry counts executions per node and fails node "d" while broken is set.
type countingRegistry struct {
	mu     sync.Mutex
	calls  map[string]int
	broken atomic.Bool
}

func (c *countingRegistry) registry() nodes.Registry {
	c.calls = map[string]int{}
	real := nodes.NewRegistry(nodes.Options{})
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		c.mu.Lock()
		c.calls[in.Node.ID]++
		c.mu.Unlock()
		if in.Node.ID == "d" && c.broken.Load() {
			return nil, &engine.NodeError{Code: engine.CodeNetwork, Message: "boom"}
		}
		return real[workflow.TypeTransform].Execute(ctx, in)
	})
	return reg
}

func (c *countingRegistry) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

// t -> a -> b -> c -> d -> e, plus a side branch t -> p.
func replayGraph() workflow.Graph {
	return build([]nd{tr("t"), tf("a", "trigger.x + 1"), tf("b", "nodes.a * 2"), tf("c", "nodes.b + 1"), tf("d", "nodes.c * 10"), tf("e", "nodes.d + 1"), tf("p", "trigger.x")},
		[3]string{"t", "a", ""}, [3]string{"a", "b", ""}, [3]string{"b", "c", ""}, [3]string{"c", "d", ""}, [3]string{"d", "e", ""}, [3]string{"t", "p", ""})
}

func failedRun(t *testing.T) (*env, *countingRegistry, *runtime.Execution) {
	t.Helper()
	e := newEnv(t)
	cr := &countingRegistry{}
	reg := cr.registry()
	cr.broken.Store(true)
	e.spawn(e.rt, procOpts{reg: reg})
	id := e.publish(replayGraph())
	ex := e.wait(e.start(id, map[string]any{"x": 4.0}).ID)
	if ex.Status != engine.ExecFailed {
		t.Fatalf("original status = %s", ex.Status)
	}
	return e, cr, ex
}

func TestReplayFromFailedNodeReusesUpstream(t *testing.T) {
	e, cr, orig := failedRun(t)
	before := map[string]int{"a": cr.count("a"), "b": cr.count("b"), "c": cr.count("c"), "d": cr.count("d"), "p": cr.count("p")}
	cr.broken.Store(false)

	res, err := e.rt.Replay(context.Background(), runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "d", CreatedBy: e.userID})
	if err != nil {
		t.Fatal(err)
	}
	re := e.wait(res.Execution.ID)
	if re.Status != engine.ExecSucceeded {
		t.Fatalf("replay status = %s err=%s", re.Status, re.Error)
	}
	if re.ReplayOf == nil || *re.ReplayOf != orig.ID || re.ReplaySourceNode != "d" || re.TriggerType != "replay" {
		t.Fatalf("lineage = %v %q %q", re.ReplayOf, re.ReplaySourceNode, re.TriggerType)
	}
	if re.VersionID != orig.VersionID {
		t.Fatal("replay must run the original version")
	}
	// Upstream nodes and the side branch were reused, d and e ran again.
	for _, id := range []string{"a", "b", "c", "p"} {
		if cr.count(id) != before[id] {
			t.Errorf("node %s re-executed (%d -> %d)", id, before[id], cr.count(id))
		}
	}
	if cr.count("d") != before["d"]+1 || cr.count("e") != 1 {
		t.Errorf("d=%d (was %d) e=%d", cr.count("d"), before["d"], cr.count("e"))
	}
	// (4+1)*2+1 = 11, d = 110, e = 111.
	if out, _ := re.Output.(map[string]any); out["e"] != 111.0 || out["p"] != 4.0 {
		t.Fatalf("output = %v", re.Output)
	}
	// The original stays untouched.
	after := e.get(orig.ID)
	if after.Status != engine.ExecFailed || after.Output != orig.Output || e.nodeState(orig.ID, "d") != engine.NodeFailed {
		t.Fatalf("original mutated: %s d=%s", after.Status, e.nodeState(orig.ID, "d"))
	}
}

func TestReplayFromMiddleRerunsEverythingDownstream(t *testing.T) {
	e, cr, orig := failedRun(t)
	cr.broken.Store(false)
	res, err := e.rt.Replay(context.Background(), runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "b"})
	if err != nil {
		t.Fatal(err)
	}
	re := e.wait(res.Execution.ID)
	if out, _ := re.Output.(map[string]any); re.Status != engine.ExecSucceeded || out["e"] != 111.0 {
		t.Fatalf("status=%s output=%v", re.Status, re.Output)
	}
	if cr.count("a") != 1 {
		t.Errorf("a re-executed: %d", cr.count("a"))
	}
	if cr.count("b") != 2 || cr.count("c") != 2 {
		t.Errorf("b=%d c=%d, want downstream rerun", cr.count("b"), cr.count("c"))
	}
}

func TestFullReplayRunsEverythingAgain(t *testing.T) {
	e, cr, orig := failedRun(t)
	cr.broken.Store(false)
	res, err := e.rt.Replay(context.Background(), runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID})
	if err != nil {
		t.Fatal(err)
	}
	re := e.wait(res.Execution.ID)
	if re.Status != engine.ExecSucceeded || re.ReplaySourceNode != "" || re.ReplayOf == nil {
		t.Fatalf("status=%s src=%q of=%v", re.Status, re.ReplaySourceNode, re.ReplayOf)
	}
	if cr.count("a") != 2 || cr.count("p") != 2 {
		t.Errorf("a=%d p=%d, want a full rerun", cr.count("a"), cr.count("p"))
	}
	if re.TriggerPayload == nil {
		t.Fatal("trigger payload not carried over")
	}
}

func TestReplayRejections(t *testing.T) {
	e, _, orig := failedRun(t)
	ctx := context.Background()
	if _, err := e.rt.Replay(ctx, runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "nope"}); !errors.Is(err, runtime.ErrNotReplayable) {
		t.Errorf("unknown node: %v", err)
	}
	// e never ran, so its upstream d did not complete; replaying from e is invalid.
	if _, err := e.rt.Replay(ctx, runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "e"}); !errors.Is(err, runtime.ErrNotReplayable) {
		t.Errorf("node behind a failure: %v", err)
	}
	if _, err := e.rt.Replay(ctx, runtime.ReplayParams{WorkspaceID: "00000000-0000-0000-0000-000000000000", ExecutionID: orig.ID}); err == nil {
		t.Error("cross-workspace replay allowed")
	}
}

func TestReplayRequiresFinishedExecution(t *testing.T) {
	e := newEnv(t) // no worker: the execution stays running
	id := e.publish(replayGraph())
	ex := e.start(id, map[string]any{"x": 1.0})
	if _, err := e.rt.Replay(context.Background(), runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: ex.ID}); !errors.Is(err, runtime.ErrNotReplayable) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayIdempotencyKey(t *testing.T) {
	e, cr, orig := failedRun(t)
	cr.broken.Store(false)
	p := runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "d", IdempotencyKey: "once"}
	r1, err := e.rt.Replay(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.rt.Replay(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Duplicate || r2.Execution.ID != r1.Execution.ID {
		t.Fatalf("duplicate=%v ids %s %s", r2.Duplicate, r1.Execution.ID, r2.Execution.ID)
	}
}

func TestReplayListedByLineage(t *testing.T) {
	e, cr, orig := failedRun(t)
	cr.broken.Store(false)
	res, err := e.rt.Replay(context.Background(), runtime.ReplayParams{WorkspaceID: e.ws, ExecutionID: orig.ID, FromNode: "d"})
	if err != nil {
		t.Fatal(err)
	}
	items, err := e.rt.List(context.Background(), runtime.ListParams{WorkspaceID: e.ws, ReplayOf: orig.ID})
	if err != nil || len(items) != 1 || items[0].ID != res.Execution.ID {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}
