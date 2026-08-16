package realtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/testutil"
)

func ev(id int64, exec, ws, typ string) runtime.Event {
	return runtime.Event{ID: id, ExecutionID: exec, WorkspaceID: ws, Type: typ}
}

func recv(t *testing.T, s *Sub) runtime.Event {
	t.Helper()
	select {
	case e := <-s.C:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return runtime.Event{}
}

func TestExecutionSubscribersOnlySeeTheirExecution(t *testing.T) {
	h := NewHub(8)
	a, b := h.Execution("a"), h.Execution("b")
	h.Dispatch([]runtime.Event{ev(1, "a", "w", runtime.EvNodeStarted), ev(2, "b", "w", runtime.EvNodeStarted)})
	if e := recv(t, a); e.ID != 1 {
		t.Fatalf("a got %d", e.ID)
	}
	if e := recv(t, b); e.ID != 2 {
		t.Fatalf("b got %d", e.ID)
	}
	if len(a.C) != 0 || len(b.C) != 0 {
		t.Fatal("unexpected extra events")
	}
}

func TestWorkspaceStreamCarriesOnlyExecutionLevelEvents(t *testing.T) {
	h := NewHub(8)
	w := h.Workspace("w1")
	other := h.Workspace("w2")
	h.Dispatch([]runtime.Event{
		ev(1, "a", "w1", runtime.EvNodeStarted),
		ev(2, "a", "w1", runtime.EvExecSucceeded),
		ev(3, "a", "", runtime.EvExecSucceeded), // no workspace: never routed
	})
	if e := recv(t, w); e.ID != 2 {
		t.Fatalf("got %d, want the execution-level event", e.ID)
	}
	if len(w.C) != 0 || len(other.C) != 0 {
		t.Fatal("leaked events")
	}
}

func TestSlowSubscriberIsDroppedWithoutBlockingPublisher(t *testing.T) {
	h := NewHub(2)
	var dropped atomic.Int32
	h.Metrics.Dropped = func() { dropped.Add(1) }
	slow := h.Execution("x")
	fast := h.Execution("x")
	go func() {
		for range fast.C {
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 1; i <= 1000; i++ {
			h.Dispatch([]runtime.Event{ev(int64(i), "x", "w", runtime.EvNodeLog)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher blocked by a slow subscriber")
	}
	select {
	case <-slow.Overflow:
	default:
		t.Fatal("slow subscriber was not flagged")
	}
	if dropped.Load() < 1 {
		t.Fatal("drop metric not recorded")
	}
	h.Unsubscribe(slow) // idempotent
	h.Unsubscribe(fast)
	if h.Len() != 0 {
		t.Fatalf("subscribers left: %d", h.Len())
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub(4)
	var clients atomic.Int32
	h.Metrics.Clients = func(d int) { clients.Add(int32(d)) }
	s := h.Execution("x")
	h.Unsubscribe(s)
	h.Unsubscribe(s)
	h.Dispatch([]runtime.Event{ev(1, "x", "w", runtime.EvNodeLog)})
	if len(s.C) != 0 || clients.Load() != 0 {
		t.Fatalf("queue=%d clients=%d", len(s.C), clients.Load())
	}
}

func TestConcurrentSubscribeDispatch(t *testing.T) {
	h := NewHub(4)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := h.Execution("x")
				h.Unsubscribe(s)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Dispatch([]runtime.Event{ev(int64(j), "x", "w", runtime.EvNodeLog)})
			}
		}()
	}
	wg.Wait()
}

func TestBusWithoutRedisDispatchesLocally(t *testing.T) {
	h := NewHub(4)
	b := NewBus(h, nil, nil)
	s := h.Execution("x")
	b.Publish([]runtime.Event{ev(1, "x", "w", runtime.EvNodeLog)})
	if e := recv(t, s); e.ID != 1 {
		t.Fatalf("got %d", e.ID)
	}
}

func TestBusDeliversAcrossProcessesViaRedis(t *testing.T) {
	rc := testutil.NewRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// "API" side subscribes; "worker" side only publishes.
	apiHub := NewHub(16)
	apiBus := NewBus(apiHub, rc, nil)
	go apiBus.Run(ctx)
	worker := NewBus(NewHub(1), rc, nil)
	worker.PublishOnly = true
	go worker.Run(ctx)

	s := apiHub.Execution("cross")
	deadline := time.Now().Add(5 * time.Second)
	// The subscription is established asynchronously; publish until seen.
	for {
		worker.Publish([]runtime.Event{ev(7, "cross", "w", runtime.EvNodeLog)})
		select {
		case e := <-s.C:
			if e.ID != 7 || e.ExecutionID != "cross" {
				t.Fatalf("got %+v", e)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("event never crossed Redis")
		}
	}
}

func TestBusPublishNeverBlocksWhenRedisIsSlow(t *testing.T) {
	rc := testutil.NewRedis(t)
	b := NewBus(NewHub(1), rc, nil) // Run is never started, so the queue only fills
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			b.Publish([]runtime.Event{ev(int64(i), "x", "w", runtime.EvNodeLog)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked")
	}
}

func TestIsTerminal(t *testing.T) {
	for _, typ := range []string{runtime.EvExecSucceeded, runtime.EvExecFailed, runtime.EvExecCancelled} {
		if !IsTerminal(typ) {
			t.Errorf("%s should be terminal", typ)
		}
	}
	if IsTerminal(runtime.EvExecStarted) || IsTerminal(runtime.EvNodeSucceeded) {
		t.Error("non-terminal reported terminal")
	}
}
