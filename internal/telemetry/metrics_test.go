package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/namesarnav/synapse/internal/runtime"
	tu "github.com/namesarnav/synapse/internal/testutil"
)

func TestObserveEventsDerivesExecutionMetrics(t *testing.T) {
	m := New()
	m.ObserveEvents([]runtime.Event{
		{Type: runtime.EvNodeRetrying},
		{Type: runtime.EvNodeRetrying},
		{Type: runtime.EvExecSucceeded, Data: map[string]any{"duration_ms": int64(1500)}},
		{Type: runtime.EvExecFailed, Data: map[string]any{"duration_ms": float64(20)}},
		{Type: runtime.EvExecFailed},
	})
	if got := testutil.ToFloat64(m.nodeRetries); got != 2 {
		t.Fatalf("retries = %v", got)
	}
	if got := testutil.ToFloat64(m.execFinished.WithLabelValues("failed")); got != 2 {
		t.Fatalf("failed = %v", got)
	}
	if got := testutil.ToFloat64(m.execFinished.WithLabelValues("succeeded")); got != 1 {
		t.Fatalf("succeeded = %v", got)
	}
	if n := testutil.CollectAndCount(m.execDuration); n != 2 {
		t.Fatalf("duration series = %d, want 2", n)
	}
}

func TestWorkerSchedulerAndHubHooks(t *testing.T) {
	m := New()
	w := m.Worker()
	w.Claimed(3)
	w.Lost()
	w.TaskDone("http_request", "succeeded", 40*time.Millisecond)
	if got := testutil.ToFloat64(m.tasksClaimed); got != 3 {
		t.Fatalf("claimed = %v", got)
	}
	if got := testutil.ToFloat64(m.leasesLost); got != 1 {
		t.Fatalf("lost = %v", got)
	}
	if got := testutil.ToFloat64(m.tasksDone.WithLabelValues("http_request", "succeeded")); got != 1 {
		t.Fatalf("done = %v", got)
	}
	m.Scheduler().Requeued(4)
	if got := testutil.ToFloat64(m.schedulerWork.WithLabelValues("requeued")); got != 4 {
		t.Fatalf("requeued = %v", got)
	}
	h := m.Hub()
	h.Clients(2)
	h.Clients(-1)
	h.Dropped()
	if got := testutil.ToFloat64(m.wsClients); got != 1 {
		t.Fatalf("clients = %v", got)
	}
	if got := testutil.ToFloat64(m.wsDropped); got != 1 {
		t.Fatalf("dropped = %v", got)
	}
}

func TestDBCollectorReportsQueueAndWorkers(t *testing.T) {
	db := tu.NewDB(t)
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO workers (id, capacity) VALUES ('w1', 4)`); err != nil {
		t.Fatal(err)
	}
	m := New()
	m.RegisterDB(db)
	n, err := testutil.GatherAndCount(m.Reg, "synapse_workers", "synapse_queue_oldest_ready_age_seconds")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("series = %d, want 5 (four worker statuses, oldest-age gauge)", n)
	}
}
