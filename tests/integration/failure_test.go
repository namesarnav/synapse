// Package integration injects failures into real Synapse processes: crashed
// workers, killed APIs and a restarted database.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/tests/stack"
)

func httpGraph(url string) map[string]any {
	return map[string]any{
		"nodes": []map[string]any{
			{"id": "t", "type": "manual_trigger", "config": map[string]any{}},
			{"id": "h", "type": "http_request", "config": map[string]any{"method": "GET", "url": url}},
		},
		"edges": []map[string]any{{"id": "t_h", "source": "t", "target": "h"}},
	}
}

func query[T any](t *testing.T, dbURL, sql string, args ...any) T {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var v T
	if err := c.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func countType(evs []stack.Event, typ, node string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ && e.NodeID == node {
			n++
		}
	}
	return n
}

// A worker that dies mid-task (SIGKILL: no release, no heartbeat) must not lose
// the work: the lease expires, the task is redelivered and a second worker
// finishes the execution exactly once.
func TestKilledWorkerTaskIsRedeliveredAndCompletesOnce(t *testing.T) {
	_, dbURL := testutil.NewDBWithURL(t)
	var hits atomic.Int32
	started := make(chan struct{}, 4)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			started <- struct{}{}
			select { // hold the first request until the worker dies
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer echo.Close()

	s := stack.New(t, dbURL)
	s.StartAPI()
	a := s.StartWorker("victim", 4)
	c := s.NewClient("kill@example.com")
	execID := c.Run(c.Publish("slow", httpGraph(echo.URL)), map[string]any{})

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatalf("worker never picked up the task\n%s", a.Logs())
	}
	a.Kill()
	killed := time.Now()
	s.StartWorker("survivor", 4)

	ex := c.Wait(execID, 30*time.Second)
	if ex.Status != "succeeded" {
		t.Fatalf("status = %s", ex.Status)
	}
	t.Logf("recovered %s after the crash", time.Since(killed).Round(10*time.Millisecond))
	if got := query[int](t, dbURL, `SELECT delivery_count FROM tasks WHERE execution_id=$1 AND node_id='h'`, execID); got != 2 {
		t.Errorf("delivery_count = %d, want 2", got)
	}
	if got := query[string](t, dbURL, `SELECT status FROM tasks WHERE execution_id=$1 AND node_id='h'`, execID); got != "succeeded" {
		t.Errorf("task status = %s", got)
	}
	evs := c.Events(execID, 0)
	if n := countType(evs, "node.succeeded", "h"); n != 1 {
		t.Errorf("node.succeeded events = %d, want exactly 1", n)
	}
	if n := countType(evs, "execution.succeeded", ""); n != 1 {
		t.Errorf("execution.succeeded events = %d, want exactly 1", n)
	}
	if hits.Load() < 2 {
		t.Errorf("echo saw %d requests; the task should have been delivered twice", hits.Load())
	}
	if got := query[string](t, dbURL, `SELECT status FROM workers WHERE id='victim'`); got == "active" {
		// The reaper marks the crashed worker dead once it stops heartbeating.
		time.Sleep(4 * time.Second)
		if got = query[string](t, dbURL, `SELECT status FROM workers WHERE id='victim'`); got == "active" {
			t.Errorf("crashed worker still marked active")
		}
	}
}

// Killing the API (which also hosts the scheduler) mid-run loses nothing:
// durable delays and queued work resume once an API is back.
func TestAPIKilledMidRunLosesNoExecutions(t *testing.T) {
	_, dbURL := testutil.NewDBWithURL(t)
	s := stack.New(t, dbURL)
	api := s.StartAPI()
	s.StartWorker("w1", 8)
	c := s.NewClient("apikill@example.com")
	wf := c.Publish("delayed", map[string]any{
		"nodes": []map[string]any{
			{"id": "t", "type": "manual_trigger", "config": map[string]any{}},
			{"id": "d", "type": "delay", "config": map[string]any{"duration_ms": 2500}},
			{"id": "x", "type": "transform", "config": map[string]any{"output": map[string]any{"v": "{{ trigger.i }}"}}},
		},
		"edges": []map[string]any{{"id": "t_d", "source": "t", "target": "d"}, {"id": "d_x", "source": "d", "target": "x"}},
	})
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		ids[i] = c.Run(wf, map[string]any{"i": i})
	}
	time.Sleep(700 * time.Millisecond)
	api.Kill()
	time.Sleep(3 * time.Second) // every delay is now overdue, and nothing is running the scheduler
	for _, id := range ids {
		if got := query[string](t, dbURL, `SELECT status FROM executions WHERE id=$1`, id); got != "running" && got != "waiting" {
			t.Fatalf("execution %s is %s while the API is down; nothing should have advanced it", id, got)
		}
	}
	s.StartAPI()
	for _, id := range ids {
		if ex := c.Wait(id, 30*time.Second); ex.Status != "succeeded" {
			t.Errorf("execution %s = %s", id, ex.Status)
		}
	}
	if got := query[int](t, dbURL, `SELECT count(*) FROM execution_events WHERE type='execution.succeeded'`); got != n {
		t.Errorf("execution.succeeded events = %d, want %d", got, n)
	}
}

// SIGTERM lets an in-flight task finish, then the worker exits cleanly.
func TestWorkerGracefulShutdownFinishesInFlightTask(t *testing.T) {
	_, dbURL := testutil.NewDBWithURL(t)
	started := make(chan struct{}, 1)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		time.Sleep(1200 * time.Millisecond)
		fmt.Fprint(w, "done")
	}))
	defer echo.Close()
	s := stack.New(t, dbURL)
	s.StartAPI()
	w := s.StartWorker("graceful", 2)
	c := s.NewClient("grace@example.com")
	execID := c.Run(c.Publish("g", httpGraph(echo.URL)), nil)
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("task never started")
	}
	begin := time.Now()
	if err := w.Stop(20 * time.Second); err != nil {
		t.Fatalf("worker exit: %v\n%s", err, w.Logs())
	}
	if time.Since(begin) < 900*time.Millisecond {
		t.Errorf("worker exited after %s, before the in-flight task could finish", time.Since(begin))
	}
	if ex := c.Wait(execID, 10*time.Second); ex.Status != "succeeded" {
		t.Errorf("status = %s", ex.Status)
	}
	if got := query[int](t, dbURL, `SELECT delivery_count FROM tasks WHERE execution_id=$1 AND node_id='h'`, execID); got != 1 {
		t.Errorf("delivery_count = %d; graceful shutdown must not cause a redelivery", got)
	}
}

func TestAPIGracefulShutdown(t *testing.T) {
	_, dbURL := testutil.NewDBWithURL(t)
	s := stack.New(t, dbURL)
	api := s.StartAPI()
	if err := api.Stop(10 * time.Second); err != nil {
		t.Fatalf("api exit: %v\n%s", err, api.Logs())
	}
	if !strings.Contains(api.Logs(), "shutting down") {
		t.Errorf("no shutdown log line:\n%s", api.Logs())
	}
}

// The database going away and coming back must not lose executions: the API
// and workers ride out the outage and finish the work.
func TestPostgresRestartIsSurvived(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const name, port = "synapse-it-pg", "55434"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-e", "POSTGRES_USER=synapse", "-e", "POSTGRES_PASSWORD=synapse",
		"-e", "POSTGRES_DB=synapse", "-p", port+":5432", "postgres:16-alpine", "postgres", "-c", "max_connections=200").CombinedOutput(); err != nil {
		t.Skipf("cannot start throwaway postgres: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	dbURL := "postgres://synapse:synapse@localhost:" + port + "/synapse?sslmode=disable"
	deadline := time.Now().Add(60 * time.Second)
	for {
		if c, err := pgx.Connect(context.Background(), dbURL); err == nil {
			var one int
			ok := c.QueryRow(context.Background(), "SELECT 1").Scan(&one) == nil
			c.Close(context.Background())
			if ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("throwaway postgres did not come up")
		}
		time.Sleep(300 * time.Millisecond)
	}

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer echo.Close()
	s := stack.New(t, dbURL)
	s.StartAPI()
	s.StartWorker("pg-w1", 8)
	c := s.NewClient("pgrestart@example.com")
	wf := c.Publish("pg", httpGraph(echo.URL))
	const n = 20
	ids := make([]string, 0, n)
	for i := 0; i < n/2; i++ {
		ids = append(ids, c.Run(wf, nil))
	}
	if out, err := exec.Command("docker", "restart", "-t", "1", name).CombinedOutput(); err != nil {
		t.Fatalf("restart postgres: %v %s", err, out)
	}
	// Submissions may fail while the database is down; retry until accepted.
	for len(ids) < n {
		var id string
		for try := 0; try < 100 && id == ""; try++ {
			var r struct{ Execution struct{ ID string } }
			if _, err := c.Do("POST", "/api/v1/workspaces/"+c.WorkspaceID+"/workflows/"+wf+"/run", map[string]any{}, &r); err == nil {
				id = r.Execution.ID
			} else {
				time.Sleep(300 * time.Millisecond)
			}
		}
		if id == "" {
			t.Fatal("API never accepted work after the database came back")
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if ex := c.Wait(id, 60*time.Second); ex.Status != "succeeded" {
			t.Errorf("execution %s = %s", id, ex.Status)
		}
	}
	if !s.API().Running() {
		t.Errorf("api process died during the database outage")
	}
}
