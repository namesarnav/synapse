package engine

import "testing"

func TestNodeTransitionsTable(t *testing.T) {
	allowed := map[[2]NodeState]bool{}
	for from, tos := range nodeTransitions {
		for _, to := range tos {
			allowed[[2]NodeState{from, to}] = true
		}
	}
	for _, from := range AllNodeStates {
		for _, to := range AllNodeStates {
			got := CanNodeTransition(from, to)
			if got != allowed[[2]NodeState{from, to}] {
				t.Errorf("%s -> %s = %v", from, to, got)
			}
			if err := CheckNodeTransition(from, to); (err == nil) != got {
				t.Errorf("Check(%s,%s) disagrees with Can", from, to)
			}
		}
	}
}

func TestTerminalNodeStatesHaveNoExits(t *testing.T) {
	for _, s := range AllNodeStates {
		if !s.Terminal() {
			continue
		}
		for _, to := range AllNodeStates {
			if CanNodeTransition(s, to) {
				t.Errorf("terminal %s can move to %s", s, to)
			}
		}
	}
}

func TestSpecificNodeTransitions(t *testing.T) {
	ok := [][2]NodeState{
		{NodePending, NodeReady}, {NodePending, NodeSkipped}, {NodeReady, NodeQueued}, {NodeQueued, NodeRunning},
		{NodeRunning, NodeSucceeded}, {NodeRunning, NodeRetrying}, {NodeRetrying, NodeRunning}, {NodeRunning, NodeQueued},
		{NodeReady, NodeWaiting}, {NodeWaiting, NodeSucceeded},
	}
	for _, p := range ok {
		if !CanNodeTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	bad := [][2]NodeState{
		{NodePending, NodeRunning}, {NodePending, NodeSucceeded}, {NodeSucceeded, NodeRunning}, {NodeFailed, NodeRetrying},
		{NodeSkipped, NodeReady}, {NodeQueued, NodeSucceeded}, {NodeWaiting, NodeRunning}, {NodeCancelled, NodeQueued},
	}
	for _, p := range bad {
		if CanNodeTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be rejected", p[0], p[1])
		}
		if err := CheckNodeTransition(p[0], p[1]); err == nil {
			t.Errorf("Check(%s,%s) accepted", p[0], p[1])
		}
	}
}

func TestExecTransitions(t *testing.T) {
	ok := [][2]ExecState{
		{ExecCreated, ExecRunning}, {ExecRunning, ExecWaiting}, {ExecWaiting, ExecRunning}, {ExecRunning, ExecSucceeded},
		{ExecRunning, ExecFailed}, {ExecRunning, ExecCancelling}, {ExecCancelling, ExecCancelled}, {ExecWaiting, ExecCancelled},
	}
	for _, p := range ok {
		if !CanExecTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	bad := [][2]ExecState{
		{ExecSucceeded, ExecRunning}, {ExecFailed, ExecRunning}, {ExecCancelled, ExecRunning}, {ExecCancelling, ExecRunning},
		{ExecCancelling, ExecSucceeded}, {ExecCreated, ExecSucceeded}, {ExecCreated, ExecWaiting},
	}
	for _, p := range bad {
		if CanExecTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be rejected", p[0], p[1])
		}
	}
	for _, s := range AllExecStates {
		if s.Terminal() {
			for _, to := range AllExecStates {
				if CanExecTransition(s, to) {
					t.Errorf("terminal %s can move to %s", s, to)
				}
			}
		}
	}
}
