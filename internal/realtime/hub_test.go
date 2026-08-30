package realtime

import (
	"testing"

	"github.com/namesarnav/synapse/internal/runtime"
)

func TestHubWorkspaceDedupesPushAndTail(t *testing.T) {
	h := NewHub(16)
	sub := h.Workspace("w1")
	e := runtime.Event{ID: 7, ExecutionID: "x", WorkspaceID: "w1", Type: runtime.EvExecSucceeded}
	h.Dispatch([]runtime.Event{e})
	h.DispatchWorkspace([]runtime.Event{e, {ID: 8, ExecutionID: "x", WorkspaceID: "w1", Type: runtime.EvExecFailed}})
	h.DispatchWorkspace([]runtime.Event{e})
	if len(sub.C) != 2 {
		t.Fatalf("delivered %d events, want 2 (7 once, 8 once)", len(sub.C))
	}
	// Events without ids are never deduplicated, and node events never reach workspace streams.
	h.Dispatch([]runtime.Event{{ExecutionID: "x", WorkspaceID: "w1", Type: runtime.EvExecStarted}, {ExecutionID: "x", WorkspaceID: "w1", Type: runtime.EvExecStarted}})
	h.DispatchWorkspace([]runtime.Event{{ID: 9, ExecutionID: "x", WorkspaceID: "w1", Type: runtime.EvNodeLog}})
	if len(sub.C) != 4 {
		t.Fatalf("delivered %d events, want 4", len(sub.C))
	}
}

func TestHubSeenSetIsBounded(t *testing.T) {
	h := NewHub(4)
	for i := int64(1); i <= seenCap*3; i++ {
		h.MarkSeen(i)
	}
	if len(h.seen) > seenCap || len(h.seenOrder) > seenCap {
		t.Fatalf("seen grew to %d/%d", len(h.seen), len(h.seenOrder))
	}
	if h.firstSeen(seenCap * 3) {
		t.Fatal("recent id should still be remembered")
	}
}
