package engine

import (
	"github.com/namesarnav/synapse/internal/workflow"
)

// Status is the engine's view of one node.
type Status struct {
	State NodeState
	// Branch is the edge label this node emitted on: "true"/"false" for
	// conditions, "done" for foreach, "" for everything else.
	Branch  string
	OnError string
}

// Statuses maps node id to status.
type Statuses map[string]*Status

func (s Statuses) Clone() Statuses {
	out := make(Statuses, len(s))
	for k, v := range s {
		c := *v
		out[k] = &c
	}
	return out
}

// EdgeState is the resolution state of an edge.
type EdgeState int

const (
	EdgeUnresolved EdgeState = iota
	EdgeActive
	EdgeDead
)

// ResolveEdge decides whether e carries data given its source's status.
func ResolveEdge(e workflow.Edge, src *Status) EdgeState {
	switch src.State {
	case NodeSucceeded:
		if e.Branch == src.Branch {
			return EdgeActive
		}
		return EdgeDead
	case NodeFailed:
		// Only a continue-on-error failure lets the flow proceed.
		if src.OnError == workflow.OnErrorContinue {
			if e.Branch == src.Branch {
				return EdgeActive
			}
			return EdgeDead
		}
		return EdgeUnresolved
	case NodeSkipped, NodeCancelled:
		return EdgeDead
	}
	return EdgeUnresolved
}

// Decision lists what the scheduler should do next.
type Decision struct {
	Ready   []string // pending nodes whose dependencies are satisfied
	Skipped []string // pending nodes with no live path (in cascade order)
}

// Resolve finds pending nodes that became ready or must be skipped. Skips are
// cascaded within one call using a topological pass. st is not modified.
func Resolve(ix *workflow.Index, order []string, st Statuses) Decision {
	work := st.Clone()
	var d Decision
	for _, id := range order {
		cur := work[id]
		if cur == nil || cur.State != NodePending {
			continue
		}
		node := ix.Nodes[id]
		if node.Type.IsTrigger() {
			continue // triggers are started explicitly
		}
		in := ix.In[id]
		if len(in) == 0 {
			continue
		}
		active, unresolved := 0, 0
		for _, e := range in {
			src := work[e.Source]
			if src == nil {
				unresolved++
				continue
			}
			switch ResolveEdge(e, src) {
			case EdgeActive:
				active++
			case EdgeUnresolved:
				unresolved++
			}
		}
		if unresolved > 0 {
			continue
		}
		if active > 0 {
			d.Ready = append(d.Ready, id)
			cur.State = NodeReady
		} else {
			d.Skipped = append(d.Skipped, id)
			cur.State = NodeSkipped
		}
	}
	return d
}

// ActiveSources returns the ids of source nodes whose edges into id are active.
func ActiveSources(ix *workflow.Index, id string, st Statuses) []string {
	var out []string
	for _, e := range ix.In[id] {
		if src := st[e.Source]; src != nil && ResolveEdge(e, src) == EdgeActive {
			out = append(out, e.Source)
		}
	}
	return out
}

// Outcome summarises whether an execution has finished.
type Outcome struct {
	Done   bool
	Failed bool // some node failed without continue-on-error
	Idle   bool // nothing is queued or running; only timers/children remain
}

// Assess inspects statuses to decide the execution's outcome.
func Assess(st Statuses) Outcome {
	var o Outcome
	busy, waiting := false, false
	all := true
	for _, s := range st {
		switch s.State {
		case NodeFailed:
			if s.OnError != workflow.OnErrorContinue {
				o.Failed = true
			}
		case NodeReady, NodeQueued, NodeRunning:
			busy = true
		case NodeWaiting, NodeRetrying:
			waiting = true
		}
		if !s.State.Terminal() {
			all = false
		}
	}
	o.Done = all
	o.Idle = !busy && waiting && !all
	return o
}

// ContinueBranch is the branch emitted when a node fails with on_error=continue.
func ContinueBranch(t workflow.NodeType) string {
	if t == workflow.TypeForEach {
		return workflow.BranchDone
	}
	return ""
}
