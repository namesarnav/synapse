package runtime_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
)

func foreachGraph(cfg map[string]any, body string) workflow.Graph {
	return build([]nd{tr("t"), {id: "fe", typ: "foreach", cfg: cfg}, tf("body", body), tf("after", "length(nodes.fe.items)")},
		[3]string{"t", "fe", ""}, [3]string{"fe", "body", "item"}, [3]string{"fe", "after", "done"})
}

func TestForEachFanOutAndJoin(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{concurrency: 6})
	id := e.publish(foreachGraph(map[string]any{"items": "trigger.xs", "concurrency": 3}, "item * 2 + index"))
	ex := e.wait(e.start(id, map[string]any{"xs": []any{10.0, 20.0, 30.0, 40.0}}).ID)
	if ex.Status != engine.ExecSucceeded || ex.Output != 4.0 {
		t.Fatalf("status=%s output=%v err=%s", ex.Status, ex.Output, ex.Error)
	}
	kids, err := e.rt.Children(context.Background(), ex.ID)
	if err != nil || len(kids) != 4 {
		t.Fatalf("children = %d err=%v", len(kids), err)
	}
	// Item outputs are collected in order in the foreach node output.
	ns, _ := e.rt.Nodes(context.Background(), ex.ID)
	for _, n := range ns {
		if n.NodeID == "fe" {
			items := n.Output.(map[string]any)["items"]
			if !reflect.DeepEqual(items, []any{20.0, 41.0, 62.0, 83.0}) {
				t.Fatalf("items = %#v", items)
			}
		}
	}
}

func TestForEachRespectsConcurrencyLimit(t *testing.T) {
	e := newEnv(t)
	var running, peak atomic.Int32
	reg := nodes.NewRegistry(nodes.Options{})
	reg[workflow.TypeTransform] = nodes.ExecutorFunc(func(ctx context.Context, in nodes.Input) (any, error) {
		if _, isItem := in.Env.Vars["item"]; !isItem {
			return 0, nil
		}
		cur := running.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		running.Add(-1)
		return in.Env.Vars["item"], nil
	})
	e.spawn(e.rt, procOpts{reg: reg, concurrency: 16})
	items := make([]any, 12)
	for i := range items {
		items[i] = float64(i)
	}
	id := e.publish(foreachGraph(map[string]any{"items": "trigger.xs", "concurrency": 3}, "item"))
	ex := e.wait(e.start(id, map[string]any{"xs": items}).ID)
	if ex.Status != engine.ExecSucceeded {
		t.Fatalf("status=%s err=%s", ex.Status, ex.Error)
	}
	if p := peak.Load(); p > 3 || p < 2 {
		t.Fatalf("peak concurrency = %d, want within [2,3]", p)
	}
}

func TestForEachEmptyItems(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{})
	id := e.publish(foreachGraph(map[string]any{"items": "trigger.xs"}, "item"))
	ex := e.wait(e.start(id, map[string]any{"xs": []any{}}).ID)
	if ex.Status != engine.ExecSucceeded || ex.Output != 0.0 {
		t.Fatalf("status=%s output=%v err=%s", ex.Status, ex.Output, ex.Error)
	}
}

func TestForEachItemFailurePolicies(t *testing.T) {
	cases := map[string]engine.ExecState{"fail": engine.ExecFailed, "continue": engine.ExecSucceeded}
	for policy, want := range cases {
		t.Run(policy, func(t *testing.T) {
			e := newEnv(t)
			e.spawn(e.rt, procOpts{})
			id := e.publish(foreachGraph(map[string]any{"items": "trigger.xs", "on_item_error": policy}, "10 / item"))
			ex := e.wait(e.start(id, map[string]any{"xs": []any{1.0, 0.0, 2.0}}).ID)
			if ex.Status != want {
				t.Fatalf("status=%s err=%s, want %s", ex.Status, ex.Error, want)
			}
		})
	}
}

func TestSubWorkflowRunsPublishedVersion(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{})
	child := e.publish(build([]nd{tr("t"), tf("x", "trigger.v + 1")}, [3]string{"t", "x", ""}))
	parent := e.publish(build([]nd{tr("t"),
		{id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": child, "input": map[string]any{"v": "{{ trigger.n }}"}}},
		tf("after", "nodes.s")},
		[3]string{"t", "s", ""}, [3]string{"s", "after", ""}))
	ex := e.wait(e.start(parent, map[string]any{"n": 41.0}).ID)
	if ex.Status != engine.ExecSucceeded || ex.Output != 42.0 {
		t.Fatalf("status=%s output=%v err=%s", ex.Status, ex.Output, ex.Error)
	}
	kids, _ := e.rt.Children(context.Background(), ex.ID)
	if len(kids) != 1 || kids[0].WorkflowID != child {
		t.Fatalf("children = %+v", kids)
	}
}

func TestSubWorkflowRecursionLimit(t *testing.T) {
	e := newEnv(t, func(rt *runtime.Runtime) { rt.MaxDepth = 3 })
	e.spawn(e.rt, procOpts{})
	// Create the workflow first so it can reference its own id.
	w, err := e.wfs.Create(context.Background(), e.ws, e.userID, "self", "", build([]nd{tr("t")}))
	if err != nil {
		t.Fatal(err)
	}
	g := build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": w.ID}}}, [3]string{"t", "s", ""})
	if _, err := e.wfs.Update(context.Background(), e.ws, w.ID, updateGraph(w.Revision, g)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.wfs.Publish(context.Background(), e.ws, w.ID, e.userID, "", func(workflow.Graph) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ex := e.wait(e.start(w.ID, nil).ID)
	if ex.Status != engine.ExecFailed {
		t.Fatalf("recursion should fail, got %s", ex.Status)
	}
}

func TestCancelCascadesToChildren(t *testing.T) {
	e := newEnv(t)
	e.spawn(e.rt, procOpts{hbEvery: 30 * time.Millisecond})
	child := e.publish(build([]nd{tr("t"), {id: "d", typ: "delay", cfg: map[string]any{"duration_ms": 60000}}}, [3]string{"t", "d", ""}))
	parent := e.publish(build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": child}}}, [3]string{"t", "s", ""}))
	ex := e.start(parent, nil)
	e.waitFor(ex.ID, func(x *runtime.Execution) bool {
		k, _ := e.rt.Children(context.Background(), x.ID)
		return len(k) == 1 && k[0].Status == engine.ExecWaiting
	})
	if err := e.rt.Cancel(context.Background(), ex.ID, "bye"); err != nil {
		t.Fatal(err)
	}
	e.wait(ex.ID)
	kids, _ := e.rt.Children(context.Background(), ex.ID)
	if len(kids) != 1 || kids[0].Status != engine.ExecCancelled {
		t.Fatalf("child = %+v", kids)
	}
}

// The parent must advance even when nobody notified it that a child finished;
// the sweeper covers that gap.
func TestSweeperAdvancesParentAfterMissedNotification(t *testing.T) {
	e := newEnv(t, func(rt *runtime.Runtime) { rt.DisableParentNotify = true })
	child := e.publish(build([]nd{tr("t"), tf("x", "1")}, [3]string{"t", "x", ""}))
	parent := e.publish(build([]nd{tr("t"), {id: "s", typ: "sub_workflow", cfg: map[string]any{"workflow_id": child}}}, [3]string{"t", "s", ""}))
	ex := e.start(parent, nil)
	e.spawn(e.rt, procOpts{noScheduler: true})
	e.waitFor(ex.ID, func(*runtime.Execution) bool {
		k, _ := e.rt.Children(context.Background(), ex.ID)
		return len(k) == 1 && k[0].Status.Terminal()
	})
	time.Sleep(300 * time.Millisecond)
	if got := e.get(ex.ID); got.Status.Terminal() {
		t.Fatalf("parent advanced without notification: %s", got.Status)
	}
	e.spawn(e.rt, procOpts{noWorker: true})
	if got := e.wait(ex.ID); got.Status != engine.ExecSucceeded {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
}
