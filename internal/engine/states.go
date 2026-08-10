// Package engine holds the pure decision logic of the workflow runtime: state
// machines, edge resolution, branching and inline node semantics. It performs
// no I/O so it can be shared by the in-process runner and the durable runtime.
package engine

import "fmt"

type NodeState string

const (
	NodePending   NodeState = "pending"
	NodeReady     NodeState = "ready"
	NodeQueued    NodeState = "queued"
	NodeRunning   NodeState = "running"
	NodeWaiting   NodeState = "waiting"
	NodeSucceeded NodeState = "succeeded"
	NodeFailed    NodeState = "failed"
	NodeRetrying  NodeState = "retrying"
	NodeSkipped   NodeState = "skipped"
	NodeCancelled NodeState = "cancelled"
)

func (s NodeState) Terminal() bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled:
		return true
	}
	return false
}

// Active reports whether the node is still in flight (not pending, not terminal).
func (s NodeState) Active() bool { return s != NodePending && !s.Terminal() }

var nodeTransitions = map[NodeState][]NodeState{
	NodePending:  {NodeReady, NodeSkipped, NodeCancelled},
	NodeReady:    {NodeQueued, NodeSucceeded, NodeWaiting, NodeFailed, NodeCancelled},
	NodeQueued:   {NodeRunning, NodeQueued, NodeFailed, NodeCancelled},
	NodeRunning:  {NodeSucceeded, NodeFailed, NodeRetrying, NodeQueued, NodeCancelled},
	NodeRetrying: {NodeRunning, NodeFailed, NodeCancelled},
	NodeWaiting:  {NodeSucceeded, NodeFailed, NodeCancelled},
}

// CanNodeTransition reports whether from -> to is allowed.
func CanNodeTransition(from, to NodeState) bool {
	for _, t := range nodeTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// NodeTransitionError is returned for a rejected transition.
type NodeTransitionError struct{ From, To NodeState }

func (e *NodeTransitionError) Error() string {
	return fmt.Sprintf("invalid node transition %s -> %s", e.From, e.To)
}

// CheckNodeTransition returns an error for an invalid transition.
func CheckNodeTransition(from, to NodeState) error {
	if !CanNodeTransition(from, to) {
		return &NodeTransitionError{from, to}
	}
	return nil
}

type ExecState string

const (
	ExecCreated    ExecState = "created"
	ExecRunning    ExecState = "running"
	ExecWaiting    ExecState = "waiting"
	ExecSucceeded  ExecState = "succeeded"
	ExecFailed     ExecState = "failed"
	ExecCancelling ExecState = "cancelling"
	ExecCancelled  ExecState = "cancelled"
)

func (s ExecState) Terminal() bool {
	return s == ExecSucceeded || s == ExecFailed || s == ExecCancelled
}

var execTransitions = map[ExecState][]ExecState{
	ExecCreated:    {ExecRunning, ExecFailed, ExecCancelled},
	ExecRunning:    {ExecWaiting, ExecSucceeded, ExecFailed, ExecCancelling, ExecCancelled},
	ExecWaiting:    {ExecRunning, ExecSucceeded, ExecFailed, ExecCancelling, ExecCancelled},
	ExecCancelling: {ExecCancelled},
}

func CanExecTransition(from, to ExecState) bool {
	for _, t := range execTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

type ExecTransitionError struct{ From, To ExecState }

func (e *ExecTransitionError) Error() string {
	return fmt.Sprintf("invalid execution transition %s -> %s", e.From, e.To)
}

func CheckExecTransition(from, to ExecState) error {
	if !CanExecTransition(from, to) {
		return &ExecTransitionError{from, to}
	}
	return nil
}

// AllNodeStates and AllExecStates are used by tests and the protocol drift check.
var AllNodeStates = []NodeState{NodePending, NodeReady, NodeQueued, NodeRunning, NodeWaiting, NodeSucceeded, NodeFailed, NodeRetrying, NodeSkipped, NodeCancelled}
var AllExecStates = []ExecState{ExecCreated, ExecRunning, ExecWaiting, ExecSucceeded, ExecFailed, ExecCancelling, ExecCancelled}
