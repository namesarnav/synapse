package runtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

func benchWorkflow(b *testing.B) (*runtime.Runtime, string, string) {
	b.Helper()
	db := testutil.NewDB(b)
	ctx := context.Background()
	u, w, err := (&auth.Store{DB: db}).Register(ctx, "b@example.com", "correct horse battery", "B", "ws")
	if err != nil {
		b.Fatal(err)
	}
	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Type: workflow.TypeManualTrigger, Config: json.RawMessage(`{}`)},
			{ID: "x", Type: workflow.TypeTransform, Config: json.RawMessage(`{"output":{"n":1}}`)},
		},
		Edges: []workflow.Edge{{ID: "e1", Source: "t", Target: "x"}},
	}
	st := &wfstore.Store{DB: db}
	wf, err := st.Create(ctx, w.ID, u.ID, "bench", "", g)
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := st.Publish(ctx, w.ID, wf.ID, u.ID, "", func(workflow.Graph) error { return nil }); err != nil {
		b.Fatal(err)
	}
	rt := runtime.New(&runtime.Runtime{DB: db, LeaseDuration: time.Minute})
	return rt, w.ID, wf.ID
}

// BenchmarkStartExecution measures creating an execution and enqueuing its first task.
func BenchmarkStartExecution(b *testing.B) {
	rt, ws, wf := benchWorkflow(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Start(ctx, runtime.StartParams{WorkspaceID: ws, WorkflowID: wf}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClaimBeginComplete measures the worker-side queue path for one task:
// claim, begin and complete (which also finishes the execution).
func BenchmarkClaimBeginComplete(b *testing.B) {
	rt, ws, wf := benchWorkflow(b)
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Start(ctx, runtime.StartParams{WorkspaceID: ws, WorkflowID: wf}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for done := 0; done < b.N; {
		tasks, err := rt.Claim(ctx, "bench", 1, nil)
		if err != nil {
			b.Fatal(err)
		}
		for _, t := range tasks {
			if _, err := rt.Begin(ctx, "bench", t); err != nil {
				b.Fatal(err)
			}
			if err := rt.Complete(ctx, t.Ref(), map[string]any{"n": 1}); err != nil {
				b.Fatal(err)
			}
			done++
		}
	}
}
