package worker

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

// The worker span must join the trace of the request that started the execution.
func TestWorkerSpanJoinsStartingTrace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	e := newEnv(t)
	ctx := context.Background()
	st := &wfstore.Store{DB: e.db}
	g := workflow.Graph{
		Nodes: []workflow.Node{node("t", workflow.TypeManualTrigger, ""), node("x", workflow.TypeTransform, `{"output":{"n":1}}`)},
		Edges: []workflow.Edge{edge("e1", "t", "x")},
	}
	w, err := st.Create(ctx, e.ws, e.user, "wf", "", g)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Publish(ctx, e.ws, w.ID, e.user, "", func(workflow.Graph) error { return nil }); err != nil {
		t.Fatal(err)
	}
	res, err := e.rt.Start(ctx, runtime.StartParams{WorkspaceID: e.ws, WorkflowID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.spawn(nodes.NewRegistry(nodes.Options{}), Config{Concurrency: 1})
	e.wait(res.Execution.ID)

	var start, node sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case "execution.start":
			start = s
		case "node x":
			node = s
		}
	}
	if start == nil || node == nil {
		t.Fatalf("missing spans: start=%v node=%v", start != nil, node != nil)
	}
	if start.SpanContext().TraceID() != node.SpanContext().TraceID() {
		t.Fatal("worker span is in a different trace")
	}
	if node.Parent().SpanID() != start.SpanContext().SpanID() {
		t.Fatal("worker span is not a child of execution.start")
	}
}
