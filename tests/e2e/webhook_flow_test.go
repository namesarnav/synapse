// Package e2e drives the real binaries through the public webhook endpoint.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/namesarnav/synapse/internal/testutil"
	"github.com/namesarnav/synapse/internal/triggers"
	"github.com/namesarnav/synapse/tests/stack"
)

// echo records the paths it is called with.
type echo struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newEcho() *echo {
	e := &echo{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.paths = append(e.paths, r.URL.Path)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	return e
}

func (e *echo) count(path string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, p := range e.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (e *echo) waitFor(t *testing.T, path string, want int, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if e.count(path) >= want {
			return
		}
	}
	t.Fatalf("%s called %d times, want %d", path, e.count(path), want)
}

// webhook → transform → condition → (true) http → delay → http | (false) stop
func orderGraph(base string, delayMS int) map[string]any {
	return map[string]any{
		"nodes": []map[string]any{
			{"id": "hook", "type": "webhook_trigger", "config": map[string]any{"hmac_secret": "HOOK_KEY"}},
			{"id": "calc", "type": "transform", "config": map[string]any{"output": map[string]any{"total": "{{ trigger.body.amount * 2 }}"}}},
			{"id": "big", "type": "condition", "config": map[string]any{"expression": "nodes.calc.total > 100"}},
			{"id": "first", "type": "http_request", "config": map[string]any{"method": "POST", "url": base + "/first", "body": map[string]any{"total": "{{ nodes.calc.total }}"}}},
			{"id": "wait", "type": "delay", "config": map[string]any{"duration_ms": delayMS}},
			{"id": "second", "type": "http_request", "config": map[string]any{"method": "GET", "url": base + "/second"}},
			{"id": "small", "type": "stop", "config": map[string]any{"status": "succeeded", "message": "small order"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "hook", "target": "calc"},
			{"id": "e2", "source": "calc", "target": "big"},
			{"id": "e3", "source": "big", "target": "first", "branch": "true"},
			{"id": "e4", "source": "first", "target": "wait"},
			{"id": "e5", "source": "wait", "target": "second"},
			{"id": "e6", "source": "big", "target": "small", "branch": "false"},
		},
	}
}

type nodeView struct {
	State  string `json:"state"`
	Output any    `json:"output"`
}

func nodes(t *testing.T, c *stack.Client, id string) map[string]nodeView {
	t.Helper()
	var r struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
			nodeView
		}
	}
	c.MustDo("GET", "/api/v1/workspaces/"+c.WorkspaceID+"/executions/"+id, nil, &r)
	out := map[string]nodeView{}
	for _, n := range r.Nodes {
		out[n.NodeID] = n.nodeView
	}
	return out
}

type hookResp struct {
	ExecutionID string `json:"execution_id"`
	Duplicate   bool   `json:"duplicate"`
}

func postHook(t *testing.T, base, id, secret string, body []byte, key string) (int, hookResp) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/hooks/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Synapse-Signature", triggers.Sign(secret, body))
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var r hookResp
	_ = json.NewDecoder(res.Body).Decode(&r)
	return res.StatusCode, r
}

func setup(t *testing.T, delayMS int) (*stack.Stack, *stack.Client, *echo, string) {
	t.Helper()
	_, dbURL := testutil.NewDBWithURL(t)
	e := newEcho()
	t.Cleanup(e.Close)
	s := stack.New(t, dbURL)
	s.StartAPI()
	s.StartWorker("e2e-w1", 8)
	s.StartWorker("e2e-w2", 8)
	c := s.NewClient("e2e@example.com")
	c.MustDo("PUT", "/api/v1/workspaces/"+c.WorkspaceID+"/secrets/HOOK_KEY", map[string]any{"value": "whsec-e2e-123456"}, nil)
	wf := c.Publish("orders", orderGraph(e.URL, delayMS))
	var list struct{ Webhooks []struct{ ID string } }
	c.MustDo("GET", "/api/v1/workspaces/"+c.WorkspaceID+"/workflows/"+wf+"/webhooks", nil, &list)
	if len(list.Webhooks) != 1 {
		t.Fatalf("webhooks = %+v", list)
	}
	return s, c, e, list.Webhooks[0].ID
}

const secret = "whsec-e2e-123456"

func TestBigOrderTakesHTTPDelayHTTPPathAndSurvivesAPIRestart(t *testing.T) {
	s, c, e, hook := setup(t, 3000)
	body := []byte(`{"amount":80}`)

	if code, _ := postHook(t, s.APIURL, hook, "", body, ""); code != 401 {
		t.Fatalf("unsigned webhook = %d, want 401", code)
	}
	code, r := postHook(t, s.APIURL, hook, secret, body, "order-1")
	if code != 202 || r.ExecutionID == "" {
		t.Fatalf("webhook = %d %+v", code, r)
	}
	id := r.ExecutionID
	if code, dup := postHook(t, s.APIURL, hook, secret, body, "order-1"); code != 202 || dup.ExecutionID != id || !dup.Duplicate {
		t.Fatalf("redelivered webhook = %d %+v; want the same execution flagged duplicate", code, dup)
	}

	// The true branch runs the first HTTP node, then parks in the delay.
	e.waitFor(t, "/first", 1, 15*time.Second)
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if nodes(t, c, id)["wait"].State == "waiting" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("delay node never waited: %+v", nodes(t, c, id))
		}
	}
	n := nodes(t, c, id)
	if n["calc"].State != "succeeded" || n["big"].State != "succeeded" || n["small"].State != "skipped" {
		t.Fatalf("wrong branch: %+v", n)
	}
	if e.count("/second") != 0 {
		t.Fatal("second HTTP call happened before the delay elapsed")
	}
	if out, _ := n["calc"].Output.(map[string]any); out["total"] != 160.0 {
		t.Errorf("calc output = %+v", n["calc"].Output)
	}

	// The wait is durable: kill the API (and its scheduler) mid-delay, start it again.
	s.API().Kill()
	time.Sleep(3500 * time.Millisecond) // longer than the remaining delay
	if e.count("/second") != 0 {
		t.Fatal("something advanced the execution with no scheduler running")
	}
	s.StartAPI()
	if ex := c.Wait(id, 30*time.Second); ex.Status != "succeeded" {
		t.Fatalf("status = %s", ex.Status)
	}
	if e.count("/first") != 1 || e.count("/second") != 1 {
		t.Errorf("calls: first=%d second=%d, want 1 and 1", e.count("/first"), e.count("/second"))
	}

	// History persists across the restart and reads back complete and ordered.
	evs := c.Events(id, 0)
	if len(evs) < 10 || evs[0].Type != "execution.created" || evs[len(evs)-1].Type != "execution.succeeded" {
		t.Fatalf("events: %d, first %q last %q", len(evs), evs[0].Type, evs[len(evs)-1].Type)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].ID <= evs[i-1].ID {
			t.Fatalf("events out of order at %d", i)
		}
	}
	var list struct{ Items []stack.Execution }
	c.MustDo("GET", "/api/v1/workspaces/"+c.WorkspaceID+"/executions", nil, &list)
	if len(list.Items) != 1 || list.Items[0].ID != id {
		t.Errorf("history = %+v", list.Items)
	}

	// Replay from the last node reuses everything upstream and calls only /second again.
	var rp struct {
		Execution struct {
			ID       string
			ReplayOf string `json:"replay_of"`
		}
	}
	c.MustDo("POST", "/api/v1/workspaces/"+c.WorkspaceID+"/executions/"+id+"/replay/second", nil, &rp)
	if rp.Execution.ID == "" || rp.Execution.ReplayOf != id {
		t.Fatalf("replay = %+v", rp)
	}
	if ex := c.Wait(rp.Execution.ID, 20*time.Second); ex.Status != "succeeded" {
		t.Fatalf("replay status = %s", ex.Status)
	}
	if e.count("/first") != 1 || e.count("/second") != 2 {
		t.Errorf("after replay: first=%d second=%d, want 1 and 2", e.count("/first"), e.count("/second"))
	}
	// A full replay starts over, including the delay.
	var full struct{ Execution struct{ ID string } }
	c.MustDo("POST", "/api/v1/workspaces/"+c.WorkspaceID+"/executions/"+id+"/replay", nil, &full)
	if ex := c.Wait(full.Execution.ID, 30*time.Second); ex.Status != "succeeded" {
		t.Fatalf("full replay status = %s", ex.Status)
	}
	if e.count("/first") != 2 || e.count("/second") != 3 {
		t.Errorf("after full replay: first=%d second=%d, want 2 and 3", e.count("/first"), e.count("/second"))
	}
}

func TestSmallOrderTakesStopBranchAndStreamsEvents(t *testing.T) {
	s, c, e, hook := setup(t, 500)
	code, r := postHook(t, s.APIURL, hook, secret, []byte(`{"amount":10}`), "")
	if code != 202 {
		t.Fatalf("webhook = %d", code)
	}
	id := r.ExecutionID

	// Stream it over the WebSocket from the start; the log is replayed, then live.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+s.APIURL[len("http"):]+"/ws/executions/"+id+"?token="+c.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var types []string
	var lastID int64
	end := ""
	for end == "" {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v (types so far %v)", err, types)
		}
		var m struct {
			Type   string
			Status string
			Event  *stack.Event
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		switch m.Type {
		case "event":
			if m.Event.ID <= lastID {
				t.Fatalf("event %d after %d", m.Event.ID, lastID)
			}
			lastID = m.Event.ID
			types = append(types, m.Event.Type)
		case "end":
			end = m.Status
		}
	}
	if end != "succeeded" || types[0] != "execution.created" || types[len(types)-1] != "execution.succeeded" {
		t.Fatalf("stream end=%q types=%v", end, types)
	}
	n := nodes(t, c, id)
	if n["small"].State != "succeeded" || n["first"].State != "skipped" || n["wait"].State != "skipped" || n["second"].State != "skipped" {
		t.Fatalf("wrong branch: %+v", n)
	}
	if e.count("/first") != 0 || e.count("/second") != 0 {
		t.Errorf("HTTP nodes ran on the untaken branch")
	}
	// The stream and the stored log agree.
	if stored := c.Events(id, 0); len(stored) != len(types) {
		t.Errorf("stored %d events, streamed %d", len(stored), len(types))
	}
	ws.Close(websocket.StatusNormalClosure, "")
}
