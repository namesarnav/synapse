package localrun

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/workflow"
)

// g builds a graph from compact node/edge specs.
type nd struct {
	id, typ string
	cfg     any
	opts    func(*workflow.Node)
}

func build(ns []nd, edges ...[3]string) *workflow.Graph {
	g := &workflow.Graph{}
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
	for _, e := range edges {
		g.Edges = append(g.Edges, workflow.Edge{Source: e[0], Target: e[1], Branch: e[2]})
	}
	return g
}

func fast() *Runner {
	return &Runner{Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
}

func tr(id string) nd { return nd{id: id, typ: "manual_trigger"} }
func tf(id string, expr string) nd {
	return nd{id: id, typ: "transform", cfg: map[string]any{"expression": expr}}
}

func run(t *testing.T, r *Runner, g *workflow.Graph, trigger any) *Result {
	t.Helper()
	if r == nil {
		r = fast()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Run(ctx, g, trigger)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestLinearChainPassesData(t *testing.T) {
	g := build([]nd{tr("t"), tf("a", "trigger.n + 1"), tf("b", "nodes.a * 2")},
		[3]string{"t", "a"}, [3]string{"a", "b"})
	res := run(t, nil, g, map[string]any{"n": 4.0})
	if res.Status != engine.ExecSucceeded || res.Output != 10.0 {
		t.Fatalf("status=%s output=%v err=%s", res.Status, res.Output, res.Error)
	}
}

// The example graph from the spec: A -> B,C ; B -> D,E ; C -> F ; D,E,F -> G.
func specDiamond() *workflow.Graph {
	return build([]nd{tr("A"), tf("B", "1"), tf("C", "2"), tf("D", "3"), tf("E", "4"), tf("F", "5"),
		{id: "G", typ: "merge"}},
		[3]string{"A", "B"}, [3]string{"A", "C"}, [3]string{"B", "D"}, [3]string{"B", "E"}, [3]string{"C", "F"},
		[3]string{"D", "G"}, [3]string{"E", "G"}, [3]string{"F", "G"})
}

func TestSpecGraphOrderingAndMerge(t *testing.T) {
	res := run(t, nil, specDiamond(), nil)
	if res.Status != engine.ExecSucceeded {
		t.Fatalf("%s %s", res.Status, res.Error)
	}
	want := map[string]any{"D": 3.0, "E": 4.0, "F": 5.0}
	if !reflect.DeepEqual(res.Output, want) {
		t.Fatalf("merge output = %#v", res.Output)
	}
	assertOrdering(t, specDiamond(), res)
}

// assertOrdering verifies no node started before every succeeded parent ended.
func assertOrdering(t *testing.T, g *workflow.Graph, res *Result) {
	t.Helper()
	endSeq, startSeq := map[string]int{}, map[string]int{}
	for _, ev := range res.Trace {
		if ev.Event == "end" {
			endSeq[ev.Node] = ev.Seq
		} else {
			startSeq[ev.Node] = ev.Seq
		}
	}
	for _, e := range g.Edges {
		s, ok := startSeq[e.Target]
		if !ok {
			continue
		}
		if res.States[e.Source] == engine.NodeSucceeded {
			end, ok := endSeq[e.Source]
			if !ok || end > s {
				t.Errorf("%s started (seq %d) before parent %s ended (seq %d)", e.Target, s, e.Source, end)
			}
		}
	}
}

func TestParallelBranchesRunConcurrently(t *testing.T) {
	var running, peak int32
	reg := nodes.Registry{workflow.TypeTransform: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		cur := atomic.AddInt32(&running, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		return in.Node.ID, nil
	})}
	g := build([]nd{tr("A"), tf("B", "1"), tf("C", "1"), tf("D", "1")},
		[3]string{"A", "B"}, [3]string{"A", "C"}, [3]string{"A", "D"})
	r := fast()
	r.Registry = reg
	res := run(t, r, g, nil)
	if res.Status != engine.ExecSucceeded {
		t.Fatal(res.Error)
	}
	if peak < 3 {
		t.Fatalf("peak parallelism = %d, want 3", peak)
	}
}

func TestMaxParallelBoundsConcurrency(t *testing.T) {
	var running, peak int32
	reg := nodes.Registry{workflow.TypeTransform: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		cur := atomic.AddInt32(&running, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		return nil, nil
	})}
	g := build([]nd{tr("A"), tf("B", "1"), tf("C", "1"), tf("D", "1"), tf("E", "1")},
		[3]string{"A", "B"}, [3]string{"A", "C"}, [3]string{"A", "D"}, [3]string{"A", "E"})
	r := fast()
	r.Registry, r.MaxParallel = reg, 2
	res := run(t, r, g, nil)
	if res.Status != engine.ExecSucceeded || peak > 2 {
		t.Fatalf("status=%s peak=%d", res.Status, peak)
	}
}

func TestConditionBranchingSkipsOtherSide(t *testing.T) {
	mk := func() *workflow.Graph {
		return build([]nd{tr("t"), {id: "c", typ: "condition", cfg: map[string]any{"expression": "trigger.ok"}},
			tf("yes", "\"y\""), tf("no", "\"n\""), {id: "after", typ: "merge"}},
			[3]string{"t", "c", ""}, [3]string{"c", "yes", "true"}, [3]string{"c", "no", "false"},
			[3]string{"yes", "after", ""}, [3]string{"no", "after", ""})
	}
	res := run(t, nil, mk(), map[string]any{"ok": true})
	if res.States["yes"] != engine.NodeSucceeded || res.States["no"] != engine.NodeSkipped || res.States["after"] != engine.NodeSucceeded {
		t.Fatalf("states = %v", res.States)
	}
	if !reflect.DeepEqual(res.Output, map[string]any{"yes": "y"}) {
		t.Fatalf("merge output = %#v", res.Output)
	}
	res = run(t, nil, mk(), map[string]any{"ok": false})
	if res.States["yes"] != engine.NodeSkipped || res.States["no"] != engine.NodeSucceeded {
		t.Fatalf("states = %v", res.States)
	}
}

func TestSkipPropagatesThroughChain(t *testing.T) {
	g := build([]nd{tr("t"), {id: "c", typ: "condition", cfg: map[string]any{"expression": "false"}},
		tf("a", "1"), tf("b", "2"), tf("c2", "3")},
		[3]string{"t", "c", ""}, [3]string{"c", "a", "true"}, [3]string{"a", "b", ""}, [3]string{"b", "c2", ""})
	res := run(t, nil, g, nil)
	for _, id := range []string{"a", "b", "c2"} {
		if res.States[id] != engine.NodeSkipped {
			t.Errorf("%s = %s, want skipped", id, res.States[id])
		}
	}
	if res.Status != engine.ExecSucceeded {
		t.Fatalf("status %s", res.Status)
	}
}

func TestJoinWaitsForAllActiveParents(t *testing.T) {
	// Slow branch must finish before the merge runs.
	slow := nd{id: "slow", typ: "delay", cfg: map[string]any{"duration_ms": 60}}
	g := build([]nd{tr("t"), slow, tf("fast", "1"), {id: "m", typ: "merge"}},
		[3]string{"t", "slow"}, [3]string{"t", "fast"}, [3]string{"slow", "m"}, [3]string{"fast", "m"})
	r := &Runner{}
	res := run(t, r, g, nil)
	out, _ := res.Output.(map[string]any)
	if _, ok := out["slow"]; !ok || res.States["m"] != engine.NodeSucceeded {
		t.Fatalf("merge ran before slow parent: %#v", res.Output)
	}
	assertOrdering(t, g, res)
}

func TestFailureStopsExecution(t *testing.T) {
	g := build([]nd{tr("t"), tf("bad", "1 / 0"), tf("after", "1")}, [3]string{"t", "bad"}, [3]string{"bad", "after"})
	res := run(t, nil, g, nil)
	if res.Status != engine.ExecFailed || res.States["bad"] != engine.NodeFailed || res.States["after"] != engine.NodeCancelled {
		t.Fatalf("status=%s states=%v", res.Status, res.States)
	}
	if res.Errors["bad"].Code != engine.CodeExpression {
		t.Errorf("error = %+v", res.Errors["bad"])
	}
}

func TestContinueOnError(t *testing.T) {
	bad := tf("bad", "1 / 0")
	bad.opts = func(n *workflow.Node) { n.OnError = workflow.OnErrorContinue }
	g := build([]nd{tr("t"), bad, tf("after", "nodes.bad.code")}, [3]string{"t", "bad"}, [3]string{"bad", "after"})
	res := run(t, nil, g, nil)
	if res.Status != engine.ExecSucceeded || res.Output != engine.CodeExpression {
		t.Fatalf("status=%s output=%v", res.Status, res.Output)
	}
	if res.States["bad"] != engine.NodeFailed {
		t.Errorf("bad = %s", res.States["bad"])
	}
}

func TestRetryWithBackoffThenSucceeds(t *testing.T) {
	var calls int32
	reg := nodes.Registry{workflow.TypeTransform: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return nil, &engine.NodeError{Code: engine.CodeNetwork, Message: "flaky", Retryable: true}
		}
		return "ok", nil
	})}
	n := tf("x", "1")
	n.opts = func(n *workflow.Node) { n.Retry = &workflow.RetryPolicy{MaxAttempts: 4, InitialDelayMS: 1} }
	g := build([]nd{tr("t"), n}, [3]string{"t", "x"})
	var delays []time.Duration
	r := &Runner{Registry: reg, Sleep: func(ctx context.Context, d time.Duration) error { delays = append(delays, d); return nil }}
	res := run(t, r, g, nil)
	if res.Status != engine.ExecSucceeded || calls != 3 || res.Attempts["x"] != 3 {
		t.Fatalf("status=%s calls=%d attempts=%d", res.Status, calls, res.Attempts["x"])
	}
	if len(delays) != 2 || delays[1] != 2*delays[0] {
		t.Errorf("delays = %v, want exponential", delays)
	}
}

func TestRetryExhaustionFails(t *testing.T) {
	var calls int32
	reg := nodes.Registry{workflow.TypeTransform: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		atomic.AddInt32(&calls, 1)
		return nil, &engine.NodeError{Code: engine.CodeNetwork, Message: "down", Retryable: true}
	})}
	n := tf("x", "1")
	n.opts = func(n *workflow.Node) { n.Retry = &workflow.RetryPolicy{MaxAttempts: 3} }
	r := fast()
	r.Registry = reg
	res := run(t, r, build([]nd{tr("t"), n}, [3]string{"t", "x"}), nil)
	if res.Status != engine.ExecFailed || calls != 3 {
		t.Fatalf("status=%s calls=%d", res.Status, calls)
	}
}

func TestNonRetryableErrorIsNotRetried(t *testing.T) {
	var calls int32
	reg := nodes.Registry{workflow.TypeTransform: nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		atomic.AddInt32(&calls, 1)
		return nil, &engine.NodeError{Code: engine.CodeHTTP, Message: "404"}
	})}
	n := tf("x", "1")
	n.opts = func(n *workflow.Node) { n.Retry = &workflow.RetryPolicy{MaxAttempts: 5} }
	r := fast()
	r.Registry = reg
	res := run(t, r, build([]nd{tr("t"), n}, [3]string{"t", "x"}), nil)
	if res.Status != engine.ExecFailed || calls != 1 {
		t.Fatalf("status=%s calls=%d", res.Status, calls)
	}
}

func TestStopNodeTerminatesExecution(t *testing.T) {
	g := build([]nd{tr("t"), {id: "s", typ: "stop", cfg: map[string]any{"status": "failed", "message": "bad {{ trigger.x }}"}}, tf("other", "1")},
		[3]string{"t", "s"}, [3]string{"t", "other"})
	res := run(t, nil, g, map[string]any{"x": "input"})
	if res.Status != engine.ExecFailed || res.Error != "bad input" {
		t.Fatalf("status=%s err=%q", res.Status, res.Error)
	}
	g = build([]nd{tr("t"), {id: "s", typ: "stop"}}, [3]string{"t", "s"})
	if res := run(t, nil, g, nil); res.Status != engine.ExecSucceeded {
		t.Fatalf("stop(success) = %s", res.Status)
	}
}

func TestForEachFanOutAndJoin(t *testing.T) {
	g := build([]nd{tr("t"),
		{id: "fe", typ: "foreach", cfg: map[string]any{"items": "trigger.xs", "concurrency": 3}},
		tf("dbl", "item * 2 + index"), tf("after", "length(nodes.fe.items)")},
		[3]string{"t", "fe", ""}, [3]string{"fe", "dbl", "item"}, [3]string{"fe", "after", "done"})
	res := run(t, nil, g, map[string]any{"xs": []any{10.0, 20.0, 30.0, 40.0}})
	if res.Status != engine.ExecSucceeded {
		t.Fatalf("%s %s", res.Status, res.Error)
	}
	fe := res.Outputs["fe"].(map[string]any)
	if !reflect.DeepEqual(fe["items"], []any{20.0, 41.0, 62.0, 83.0}) {
		t.Errorf("items = %#v", fe["items"])
	}
	if res.Output != 4.0 || res.States["dbl"] != engine.NodeSkipped {
		t.Errorf("output=%v dbl=%s", res.Output, res.States["dbl"])
	}
}

func TestForEachConcurrencyLimitAndEmpty(t *testing.T) {
	var running, peak int32
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		cur := atomic.AddInt32(&running, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		return in.Env.Vars["item"], nil
	})
	items := make([]any, 40)
	for i := range items {
		items[i] = float64(i)
	}
	g := build([]nd{tr("t"), {id: "fe", typ: "foreach", cfg: map[string]any{"items": "trigger.xs", "concurrency": 4}}, tf("b", "1")},
		[3]string{"t", "fe", ""}, [3]string{"fe", "b", "item"})
	r := fast()
	r.Registry = reg
	res := run(t, r, g, map[string]any{"xs": items})
	if res.Status != engine.ExecSucceeded || peak > 4 || peak < 2 {
		t.Fatalf("status=%s peak=%d", res.Status, peak)
	}
	res = run(t, nil, g, map[string]any{"xs": []any{}})
	if res.Status != engine.ExecSucceeded || res.Outputs["fe"].(map[string]any)["count"] != 0.0 {
		t.Fatalf("empty foreach: %s %v", res.Status, res.Outputs["fe"])
	}
}

func TestForEachPartialFailure(t *testing.T) {
	mk := func(policy string) *workflow.Graph {
		return build([]nd{tr("t"), {id: "fe", typ: "foreach", cfg: map[string]any{"items": "trigger.xs", "on_item_error": policy}},
			tf("b", "10 / item")}, [3]string{"t", "fe", ""}, [3]string{"fe", "b", "item"})
	}
	in := map[string]any{"xs": []any{1.0, 0.0, 2.0}}
	res := run(t, nil, mk("continue"), in)
	fe := res.Outputs["fe"].(map[string]any)
	if res.Status != engine.ExecSucceeded || fe["failed"] != 1.0 {
		t.Fatalf("continue: %s %v", res.Status, fe)
	}
	items := fe["items"].([]any)
	if items[0] != 10.0 || items[2] != 5.0 {
		t.Errorf("items = %v", items)
	}
	res = run(t, nil, mk("fail"), in)
	if res.Status != engine.ExecFailed || res.States["fe"] != engine.NodeFailed {
		t.Fatalf("fail: %s %v", res.Status, res.States)
	}
}

func TestForEachBodyCanReadOuterNodes(t *testing.T) {
	g := build([]nd{tr("t"), tf("cfg", "{mult: 3}"), {id: "fe", typ: "foreach", cfg: map[string]any{"items": "[1,2]"}},
		tf("b", "item * nodes.cfg.mult")}, [3]string{"t", "cfg"}, [3]string{"cfg", "fe", ""}, [3]string{"fe", "b", "item"})
	res := run(t, nil, g, nil)
	if res.Status != engine.ExecSucceeded {
		t.Fatalf("%s %s", res.Status, res.Error)
	}
	if !reflect.DeepEqual(res.Outputs["fe"].(map[string]any)["items"], []any{3.0, 6.0}) {
		t.Errorf("items = %v", res.Outputs["fe"])
	}
}

func TestForEachMaxItems(t *testing.T) {
	g := build([]nd{tr("t"), {id: "fe", typ: "foreach", cfg: map[string]any{"items": "range(50)", "max_items": 10}}, tf("b", "1")},
		[3]string{"t", "fe", ""}, [3]string{"fe", "b", "item"})
	if res := run(t, nil, g, nil); res.Status != engine.ExecFailed {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestDelayWaitsThenContinues(t *testing.T) {
	g := build([]nd{tr("t"), {id: "d", typ: "delay", cfg: map[string]any{"duration_ms": 50}}, tf("after", "1")},
		[3]string{"t", "d"}, [3]string{"d", "after"})
	start := time.Now()
	res := run(t, &Runner{}, g, nil)
	if res.Status != engine.ExecSucceeded || time.Since(start) < 45*time.Millisecond {
		t.Fatalf("status=%s elapsed=%v", res.Status, time.Since(start))
	}
}

func TestSubWorkflowAndRecursionLimit(t *testing.T) {
	child := build([]nd{tr("t"), tf("x", "trigger.v + 1")}, [3]string{"t", "x"})
	self := build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": "self", "input": "{{ trigger }}"}}},
		[3]string{"t", "s"})
	parent := build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": "child", "input": map[string]any{"v": "{{ trigger.n }}"}}}},
		[3]string{"t", "s"})
	r := fast()
	r.Workflows = func(id string) (*workflow.Graph, error) {
		switch id {
		case "child":
			c := *child
			return &c, nil
		case "self":
			c := *self
			return &c, nil
		}
		return nil, fmt.Errorf("workflow %s not found", id)
	}
	res := run(t, r, parent, map[string]any{"n": 41.0})
	if res.Status != engine.ExecSucceeded || res.Output != 42.0 {
		t.Fatalf("status=%s output=%v err=%s", res.Status, res.Output, res.Error)
	}
	r.MaxDepth = 3
	res = run(t, r, self, map[string]any{})
	if res.Status != engine.ExecFailed {
		t.Fatalf("recursion should fail, got %s", res.Status)
	}
	bad := build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": "nope"}}}, [3]string{"t", "s"})
	if res := run(t, r, bad, nil); res.Status != engine.ExecFailed {
		t.Fatalf("missing child should fail, got %s", res.Status)
	}
}

func TestCancellation(t *testing.T) {
	g := build([]nd{tr("t"), {id: "d", typ: "delay", cfg: map[string]any{"duration_ms": 60000}}, tf("after", "1")},
		[3]string{"t", "d"}, [3]string{"d", "after"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, err := (&Runner{}).Run(ctx, g, nil)
	if err != nil || res.Status != engine.ExecCancelled {
		t.Fatalf("status=%v err=%v", res, err)
	}
	if res.States["d"] != engine.NodeCancelled || res.States["after"] != engine.NodeCancelled {
		t.Errorf("states = %v", res.States)
	}
}

func TestMultipleTriggersOnlyChosenFires(t *testing.T) {
	g := build([]nd{tr("a"), {id: "b", typ: "webhook_trigger"}, tf("x", "1"), tf("y", "2"), {id: "m", typ: "merge"}},
		[3]string{"a", "x"}, [3]string{"b", "y"}, [3]string{"x", "m"}, [3]string{"y", "m"})
	res := run(t, nil, g, nil)
	if res.States["b"] != engine.NodeSkipped || res.States["y"] != engine.NodeSkipped || res.States["m"] != engine.NodeSucceeded {
		t.Fatalf("states = %v", res.States)
	}
}

func TestInvalidGraphRejected(t *testing.T) {
	g := build([]nd{tr("t"), tf("a", "1"), tf("b", "1")}, [3]string{"t", "a"}, [3]string{"a", "b"}, [3]string{"b", "a"})
	if _, err := fast().Run(context.Background(), g, nil); err == nil {
		t.Fatal("cyclic graph accepted")
	}
}

// Random DAGs of transforms: every node must run, respect ordering, and the
// value flowing through sums must be deterministic.
func TestRandomDAGsOrderingProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 60; iter++ {
		n := 2 + rng.Intn(25)
		ns := []nd{tr("n0")}
		var edges [][3]string
		for i := 1; i < n; i++ {
			id := fmt.Sprintf("n%d", i)
			ns = append(ns, tf(id, fmt.Sprintf("%d", i)))
			parents := map[int]bool{rng.Intn(i): true}
			for k := rng.Intn(3); k > 0; k-- {
				parents[rng.Intn(i)] = true
			}
			for p := range parents {
				edges = append(edges, [3]string{fmt.Sprintf("n%d", p), id, ""})
			}
		}
		g := build(ns, edges...)
		res := run(t, nil, g, nil)
		if res.Status != engine.ExecSucceeded {
			t.Fatalf("iter %d: %s %s", iter, res.Status, res.Error)
		}
		for id, s := range res.States {
			if s != engine.NodeSucceeded {
				t.Fatalf("iter %d: %s = %s", iter, id, s)
			}
		}
		assertOrdering(t, g, res)
	}
}
