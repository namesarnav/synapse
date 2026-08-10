package engine

import (
	"encoding/json"
	"testing"

	"github.com/namesarnav/synapse/internal/workflow"
)

func graph(nodes map[string]workflow.NodeType, edges ...workflow.Edge) (*workflow.Index, []string) {
	g := &workflow.Graph{}
	for id, t := range nodes {
		g.Nodes = append(g.Nodes, workflow.Node{ID: id, Type: t, Config: json.RawMessage(`{}`), OnError: workflow.OnErrorFail})
	}
	g.Edges = edges
	ix := workflow.NewIndex(g)
	order, _ := ix.TopoOrder()
	return ix, order
}

func statuses(m map[string]NodeState) Statuses {
	s := Statuses{}
	for k, v := range m {
		s[k] = &Status{State: v, OnError: workflow.OnErrorFail}
	}
	return s
}

func ids(x []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range x {
		m[s] = true
	}
	return m
}

func TestResolveReadyWhenAllParentsDone(t *testing.T) {
	ix, order := graph(map[string]workflow.NodeType{"t": "manual_trigger", "a": "transform", "b": "transform", "j": "merge"},
		workflow.Edge{Source: "t", Target: "a"}, workflow.Edge{Source: "t", Target: "b"},
		workflow.Edge{Source: "a", Target: "j"}, workflow.Edge{Source: "b", Target: "j"})
	st := statuses(map[string]NodeState{"t": NodeSucceeded, "a": NodePending, "b": NodePending, "j": NodePending})
	d := Resolve(ix, order, st)
	if got := ids(d.Ready); !got["a"] || !got["b"] || got["j"] || len(d.Skipped) != 0 {
		t.Fatalf("decision = %+v", d)
	}
	st["a"].State, st["b"].State = NodeSucceeded, NodeRunning
	if d := Resolve(ix, order, st); len(d.Ready) != 0 {
		t.Fatalf("join must wait for running parent: %+v", d)
	}
	st["b"].State = NodeSucceeded
	if d := Resolve(ix, order, st); len(d.Ready) != 1 || d.Ready[0] != "j" {
		t.Fatalf("join should be ready: %+v", d)
	}
}

func TestResolveSkipCascadeAndJoinWithOneLiveParent(t *testing.T) {
	ix, order := graph(map[string]workflow.NodeType{"t": "manual_trigger", "c": "condition", "x": "transform", "y": "transform", "z": "transform", "j": "merge"},
		workflow.Edge{Source: "t", Target: "c"},
		workflow.Edge{Source: "c", Target: "x", Branch: "true"}, workflow.Edge{Source: "c", Target: "y", Branch: "false"},
		workflow.Edge{Source: "x", Target: "z"}, workflow.Edge{Source: "z", Target: "j"}, workflow.Edge{Source: "y", Target: "j"})
	st := statuses(map[string]NodeState{"t": NodeSucceeded, "c": NodeSucceeded, "x": NodePending, "y": NodePending, "z": NodePending, "j": NodePending})
	st["c"].Branch = "false"
	d := Resolve(ix, order, st)
	if got := ids(d.Skipped); !got["x"] || !got["z"] || len(d.Skipped) != 2 {
		t.Fatalf("skipped = %v", d.Skipped)
	}
	if got := ids(d.Ready); !got["y"] || got["j"] {
		t.Fatalf("ready = %v", d.Ready)
	}
	if st["x"].State != NodePending {
		t.Error("Resolve must not mutate its input")
	}
}

func TestResolveAllParentsDeadSkips(t *testing.T) {
	ix, order := graph(map[string]workflow.NodeType{"t": "manual_trigger", "c": "condition", "a": "transform", "b": "transform", "j": "merge"},
		workflow.Edge{Source: "t", Target: "c"}, workflow.Edge{Source: "c", Target: "a", Branch: "true"}, workflow.Edge{Source: "c", Target: "b", Branch: "true"},
		workflow.Edge{Source: "a", Target: "j"}, workflow.Edge{Source: "b", Target: "j"})
	st := statuses(map[string]NodeState{"t": NodeSucceeded, "c": NodeSucceeded, "a": NodePending, "b": NodePending, "j": NodePending})
	st["c"].Branch = "false"
	d := Resolve(ix, order, st)
	if len(d.Skipped) != 3 || len(d.Ready) != 0 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFailedNodeBlocksUnlessContinue(t *testing.T) {
	e := workflow.Edge{Source: "a", Target: "b"}
	st := &Status{State: NodeFailed, OnError: workflow.OnErrorFail}
	if ResolveEdge(e, st) != EdgeUnresolved {
		t.Error("failed node must not resolve edges")
	}
	st.OnError = workflow.OnErrorContinue
	if ResolveEdge(e, st) != EdgeActive {
		t.Error("continue-on-error should activate unlabeled edges")
	}
	if ResolveEdge(workflow.Edge{Source: "a", Target: "b", Branch: "true"}, st) != EdgeDead {
		t.Error("labeled edges of a failed node must be dead")
	}
	if ResolveEdge(e, &Status{State: NodeCancelled}) != EdgeDead || ResolveEdge(e, &Status{State: NodeRunning}) != EdgeUnresolved {
		t.Error("cancelled = dead, running = unresolved")
	}
}

func TestAssess(t *testing.T) {
	if o := Assess(statuses(map[string]NodeState{"a": NodeSucceeded, "b": NodeSkipped})); !o.Done || o.Failed {
		t.Errorf("%+v", o)
	}
	if o := Assess(statuses(map[string]NodeState{"a": NodeSucceeded, "b": NodeFailed})); !o.Done || !o.Failed {
		t.Errorf("%+v", o)
	}
	if o := Assess(statuses(map[string]NodeState{"a": NodeSucceeded, "b": NodeWaiting})); o.Done || !o.Idle {
		t.Errorf("%+v", o)
	}
	if o := Assess(statuses(map[string]NodeState{"a": NodeRunning, "b": NodeWaiting})); o.Idle {
		t.Errorf("%+v", o)
	}
}
