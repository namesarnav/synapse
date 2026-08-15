package triggers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

type env struct {
	t    *testing.T
	db   *persistence.DB
	rt   *runtime.Runtime
	wf   *wfstore.Store
	ws   string
	user string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testutil.NewDB(t)
	u, ws, err := (&auth.Store{DB: db}).Register(context.Background(), "t@example.com", "correct-horse-battery", "T", "w")
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(&runtime.Runtime{DB: db})
	return &env{t: t, db: db, rt: rt, wf: &wfstore.Store{DB: db}, ws: ws.ID, user: u.ID}
}

func (e *env) store() *Store { return &Store{DB: e.db, RT: e.rt} }

func node(id string, typ workflow.NodeType, cfg string) workflow.Node {
	n := workflow.Node{ID: id, Type: typ}
	if cfg != "" {
		n.Config = json.RawMessage(cfg)
	}
	return n
}

func (e *env) publish(g workflow.Graph) string {
	e.t.Helper()
	ctx := context.Background()
	w, err := e.wf.Create(ctx, e.ws, e.user, "wf", "", g)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.wf.Publish(ctx, e.ws, w.ID, e.user, "", func(workflow.Graph) error { return nil }); err != nil {
		e.t.Fatal(err)
	}
	if err := e.store().Sync(ctx, e.ws, w.ID); err != nil {
		e.t.Fatal(err)
	}
	return w.ID
}

func scheduled(cron string) workflow.Graph {
	return workflow.Graph{
		Nodes: []workflow.Node{node("s", workflow.TypeScheduleTrigger, `{"cron":"`+cron+`","payload":{"k":"v"}}`), node("l", workflow.TypeLog, `{"message":"tick"}`)},
		Edges: []workflow.Edge{{ID: "e1", Source: "s", Target: "l"}},
	}
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSyncCreatesAndRemovesSchedules(t *testing.T) {
	e := newEnv(t)
	id := e.publish(scheduled("*/5 * * * *"))
	if n := e.count(`SELECT count(*) FROM schedules WHERE workflow_id=$1 AND next_run_at > now()`, id); n != 1 {
		t.Fatalf("schedules = %d", n)
	}
	// Re-sync keeps the pending fire time.
	var before time.Time
	_ = e.db.Pool.QueryRow(context.Background(), `SELECT next_run_at FROM schedules WHERE workflow_id=$1`, id).Scan(&before)
	if err := e.store().Sync(context.Background(), e.ws, id); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	_ = e.db.Pool.QueryRow(context.Background(), `SELECT next_run_at FROM schedules WHERE workflow_id=$1`, id).Scan(&after)
	if !before.Equal(after) {
		t.Fatalf("next_run_at moved: %v -> %v", before, after)
	}
	if err := e.wf.Unpublish(context.Background(), e.ws, id); err != nil {
		t.Fatal(err)
	}
	if err := e.store().Sync(context.Background(), e.ws, id); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM schedules WHERE workflow_id=$1`, id); n != 0 {
		t.Fatalf("schedules after unpublish = %d", n)
	}
}

func TestWebhookEndpointsAreStable(t *testing.T) {
	e := newEnv(t)
	g := workflow.Graph{
		Nodes: []workflow.Node{node("h", workflow.TypeWebhookTrigger, `{"hmac_secret":"HOOK_KEY"}`), node("l", workflow.TypeLog, `{"message":"x"}`)},
		Edges: []workflow.Edge{{ID: "e1", Source: "h", Target: "l"}},
	}
	id := e.publish(g)
	ctx := context.Background()
	eps, err := e.store().Endpoints(ctx, e.ws, id)
	if err != nil || len(eps) != 1 || eps[0].HMACSecret != "HOOK_KEY" {
		t.Fatalf("endpoints = %+v %v", eps, err)
	}
	// Unpublish and republish: same URL.
	_ = e.wf.Unpublish(ctx, e.ws, id)
	_ = e.store().Sync(ctx, e.ws, id)
	if _, _, err := e.wf.Publish(ctx, e.ws, id, e.user, "", func(workflow.Graph) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_ = e.store().Sync(ctx, e.ws, id)
	again, _ := e.store().Endpoints(ctx, e.ws, id)
	if len(again) != 1 || again[0].ID != eps[0].ID {
		t.Fatalf("endpoint id changed: %+v vs %+v", eps, again)
	}
	if _, err := e.store().Endpoint(ctx, "not-a-uuid"); err != ErrNoEndpoint {
		t.Fatalf("bad id: %v", err)
	}
}

func TestFireDueCompetingSchedulersFireOnce(t *testing.T) {
	e := newEnv(t)
	id := e.publish(scheduled("* * * * *"))
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `UPDATE schedules SET next_run_at = now() - interval '3 hours' WHERE workflow_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	total := 0
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.store().FireDue(ctx, 10)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("fired %d times, want 1", total)
	}
	// Downtime collapses to one run, not one per missed minute.
	if n := e.count(`SELECT count(*) FROM executions WHERE workflow_id=$1`, id); n != 1 {
		t.Fatalf("executions = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM schedules WHERE workflow_id=$1 AND next_run_at > now() AND last_run_at IS NOT NULL`, id); n != 1 {
		t.Fatal("schedule was not advanced")
	}
	var trig []byte
	var typ string
	_ = e.db.Pool.QueryRow(ctx, `SELECT trigger_type, trigger_payload FROM executions WHERE workflow_id=$1`, id).Scan(&typ, &trig)
	var p map[string]any
	_ = json.Unmarshal(trig, &p)
	if typ != "schedule" || p["payload"].(map[string]any)["k"] != "v" || p["scheduled_for"] == nil {
		t.Fatalf("trigger = %s %s", typ, trig)
	}
}

func TestFireDueRecoversFromCrashWithoutDuplicate(t *testing.T) {
	e := newEnv(t)
	id := e.publish(scheduled("* * * * *"))
	ctx := context.Background()
	past := time.Now().Add(-10 * time.Minute).Truncate(time.Second).UTC()
	_, _ = e.db.Pool.Exec(ctx, `UPDATE schedules SET next_run_at=$2 WHERE workflow_id=$1`, id, past)
	// Simulate a scheduler that started the run but died before advancing the row.
	key := "schedule:" + id + ":s:" + itoa(past.Unix())
	if _, err := e.rt.Start(ctx, runtime.StartParams{WorkspaceID: e.ws, WorkflowID: id, TriggerType: "schedule", StartNode: "s", IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store().FireDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM executions WHERE workflow_id=$1`, id); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM schedules WHERE workflow_id=$1 AND next_run_at > now()`, id); n != 1 {
		t.Fatal("schedule not advanced after recovery")
	}
}

func TestFireDueSkipsUnpublished(t *testing.T) {
	e := newEnv(t)
	id := e.publish(scheduled("* * * * *"))
	ctx := context.Background()
	_, _ = e.db.Pool.Exec(ctx, `UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE workflow_id=$1`, id)
	_ = e.wf.Unpublish(ctx, e.ws, id) // no Sync: the join alone must protect us
	n, err := e.store().FireDue(ctx, 10)
	if err != nil || n != 0 {
		t.Fatalf("fired %d, %v", n, err)
	}
}

func TestSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	sig := Sign("k", body)
	if !VerifySignature("k", body, sig) || !VerifySignature("k", body, sig[len("sha256="):]) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("other", body, sig) || VerifySignature("k", []byte("x"), sig) || VerifySignature("k", body, "") {
		t.Fatal("invalid signature accepted")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
