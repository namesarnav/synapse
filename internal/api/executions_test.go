package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/worker"
)

func (h *harness) startEngine() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	rt := h.Srv.Runtime
	go func() {
		worker.New(rt, nodes.NewRegistry(nodes.Options{}), worker.Config{Concurrency: 4, PollInterval: 20 * time.Millisecond, ShutdownGrace: time.Second}, nil).Run(ctx)
		done <- struct{}{}
	}()
	go func() {
		scheduler.New(rt, scheduler.Config{WakeInterval: 20 * time.Millisecond, ReapInterval: 50 * time.Millisecond}, nil).Run(ctx)
		done <- struct{}{}
	}()
	h.t.Cleanup(func() { cancel(); <-done; <-done })
}

func (h *harness) publishSimple(a account) string {
	h.t.Helper()
	var wf struct{ ID string }
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "runme", "graph": simpleGraph}).JSON(h.t, &wf)
	if r := h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, map[string]any{}); r.Status != 200 && r.Status != 201 {
		h.t.Fatalf("publish: %d %s", r.Status, r.Body)
	}
	return wf.ID
}

func (a account) ex(path string) string {
	return fmt.Sprintf("/api/v1/workspaces/%s/executions%s", a.WorkspaceID, path)
}

func TestRunWorkflowEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.startEngine()
	a := h.register("run@example.com")
	id := h.publishSimple(a)

	r := h.do("POST", a.wf("/"+id+"/run"), a.Token, map[string]any{"trigger": map[string]any{"x": 1}})
	if r.Status != 202 {
		t.Fatalf("run: %d %s", r.Status, r.Body)
	}
	var started struct {
		Execution runtime.Execution `json:"execution"`
	}
	r.JSON(t, &started)
	var got struct {
		Execution runtime.Execution  `json:"execution"`
		Nodes     []runtime.NodeExec `json:"nodes"`
		Attempts  []runtime.Attempt  `json:"attempts"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.do("GET", a.ex("/"+started.Execution.ID), a.Token, nil).JSON(t, &got)
		if got.Execution.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout, status %s", got.Execution.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Execution.Status != "succeeded" || len(got.Nodes) != 2 || len(got.Attempts) != 1 {
		t.Fatalf("unexpected result %+v", got)
	}

	var list struct {
		Items []runtime.Summary `json:"items"`
	}
	h.do("GET", a.ex("?workflow_id="+id+"&status=succeeded"), a.Token, nil).JSON(t, &list)
	if len(list.Items) != 1 {
		t.Fatalf("list = %+v", list)
	}
	var evs struct{ Events []runtime.Event }
	h.do("GET", a.ex("/"+started.Execution.ID+"/events"), a.Token, nil).JSON(t, &evs)
	if len(evs.Events) < 4 {
		t.Fatalf("events = %d", len(evs.Events))
	}
}

func TestRunIdempotencyKeyHeader(t *testing.T) {
	h := newHarness(t)
	a := h.register("idem@example.com")
	id := h.publishSimple(a)
	first := h.doHeaders("POST", a.wf("/"+id+"/run"), a.Token, map[string]any{}, map[string]string{"Idempotency-Key": "abc"})
	second := h.doHeaders("POST", a.wf("/"+id+"/run"), a.Token, map[string]any{}, map[string]string{"Idempotency-Key": "abc"})
	if first.Status != 202 || second.Status != 200 {
		t.Fatalf("statuses %d %d", first.Status, second.Status)
	}
	var a1, a2 struct{ Execution struct{ ID string } }
	first.JSON(t, &a1)
	second.JSON(t, &a2)
	if a1.Execution.ID != a2.Execution.ID {
		t.Fatal("different executions for the same key")
	}
}

func TestRunErrors(t *testing.T) {
	h := newHarness(t)
	a := h.register("err@example.com")
	b := h.register("other@example.com")
	var wf struct{ ID string }
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "draft", "graph": simpleGraph}).JSON(t, &wf)
	if r := h.do("POST", a.wf("/"+wf.ID+"/run"), a.Token, nil); r.Status != 409 {
		t.Fatalf("unpublished run = %d", r.Status)
	}
	id := h.publishSimple(a)
	if r := h.do("POST", a.wf("/"+id+"/run"), b.Token, nil); r.Status != 404 {
		t.Fatalf("cross-workspace run = %d", r.Status)
	}
	if r := h.do("GET", a.ex("/00000000-0000-0000-0000-000000000000"), a.Token, nil); r.Status != 404 {
		t.Fatalf("missing execution = %d", r.Status)
	}
	if r := h.do("GET", a.ex("/nope"), a.Token, nil); r.Status != 404 {
		t.Fatalf("bad id = %d", r.Status)
	}
	var ex struct{ Execution struct{ ID string } }
	h.do("POST", a.wf("/"+id+"/run"), a.Token, nil).JSON(t, &ex)
	// Another workspace must not see the execution.
	if r := h.do("GET", b.ex("/"+ex.Execution.ID), b.Token, nil); r.Status != 404 {
		t.Fatalf("cross-tenant read = %d", r.Status)
	}
	if r := h.do("POST", b.ex("/"+ex.Execution.ID+"/cancel"), b.Token, nil); r.Status != 404 {
		t.Fatalf("cross-tenant cancel = %d", r.Status)
	}
	if r := h.do("POST", a.ex("/"+ex.Execution.ID+"/cancel"), a.Token, nil); r.Status != 200 {
		t.Fatalf("cancel = %d %s", r.Status, r.Body)
	}
	if r := h.do("POST", a.ex("/"+ex.Execution.ID+"/cancel"), a.Token, nil); r.Status != 409 {
		t.Fatalf("second cancel = %d", r.Status)
	}
}

func TestQueueFullReturns503(t *testing.T) {
	h := newHarness(t)
	h.Srv.Runtime.MaxQueueDepth = 1
	a := h.register("q@example.com")
	id := h.publishSimple(a)
	if r := h.do("POST", a.wf("/"+id+"/run"), a.Token, nil); r.Status != 202 {
		t.Fatalf("first = %d", r.Status)
	}
	r := h.do("POST", a.wf("/"+id+"/run"), a.Token, nil)
	if r.Status != 503 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("second = %d retry-after=%q", r.Status, r.Header.Get("Retry-After"))
	}
}
