package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
)

func (h *harness) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(h.TS.URL, "http") + path
}

func (h *harness) dialWS(path, token string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if hdr == nil {
		hdr = http.Header{}
	}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	c, res, err := websocket.Dial(ctx, h.wsURL(path), &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		h.t.Cleanup(func() { c.CloseNow() })
	}
	return c, res, err
}

func readMsg(t *testing.T, c *websocket.Conn) realtime.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var m realtime.Message
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// drain reads until the end message and returns every event received.
func drain(t *testing.T, c *websocket.Conn) (first realtime.Message, evs []runtime.Event, end realtime.Message) {
	t.Helper()
	first = readMsg(t, c)
	for {
		m := readMsg(t, c)
		switch m.Type {
		case realtime.MsgEvent:
			evs = append(evs, *m.Event)
		case realtime.MsgEnd:
			return first, evs, m
		default:
			t.Fatalf("unexpected message %+v", m)
		}
	}
}

func (h *harness) runOnce(a account, wf string) string {
	h.t.Helper()
	r := h.do("POST", a.wf("/"+wf+"/run"), a.Token, map[string]any{})
	if r.Status != 202 {
		h.t.Fatalf("run: %d %s", r.Status, r.Body)
	}
	var s struct {
		Execution runtime.Execution `json:"execution"`
	}
	r.JSON(h.t, &s)
	return s.Execution.ID
}

func TestWSStreamsReplayLiveAndEnd(t *testing.T) {
	h := newHarness(t)
	a := h.register("ws1@example.com")
	wf := h.publishSimple(a)
	id := h.runOnce(a, wf) // no engine yet: only execution.created exists

	c, _, err := h.dialWS("/ws/executions/"+id, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	hello := readMsg(t, c)
	if hello.Type != realtime.MsgHello || hello.ExecutionID != id {
		t.Fatalf("hello = %+v", hello)
	}
	replay := readMsg(t, c)
	if replay.Type != realtime.MsgEvent || replay.Event.Type != runtime.EvExecCreated {
		t.Fatalf("replay = %+v", replay)
	}

	h.startEngine() // the rest arrives live
	var got []runtime.Event
	got = append(got, *replay.Event)
	var end realtime.Message
	for {
		m := readMsg(t, c)
		if m.Type == realtime.MsgEnd {
			end = m
			break
		}
		if m.Type != realtime.MsgEvent {
			t.Fatalf("unexpected %+v", m)
		}
		got = append(got, *m.Event)
	}
	if end.Status != "succeeded" {
		t.Fatalf("end = %+v", end)
	}
	last := got[len(got)-1]
	if last.Type != runtime.EvExecSucceeded || end.LastEventID != last.ID {
		t.Fatalf("last = %+v end = %+v", last, end)
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Fatalf("events out of order: %d after %d", got[i].ID, got[i-1].ID)
		}
	}
}

func TestWSResumeWithAfterSkipsSeenEvents(t *testing.T) {
	h := newHarness(t)
	h.startEngine()
	a := h.register("ws2@example.com")
	id := h.runOnce(a, h.publishSimple(a))

	c, _, err := h.dialWS("/ws/executions/"+id, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, all, _ := drain(t, c)
	if len(all) < 4 {
		t.Fatalf("only %d events", len(all))
	}
	cut := all[1].ID
	c2, _, err := h.dialWS("/ws/executions/"+id+"?after="+strconv.FormatInt(cut, 10), a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, end := drain(t, c2)
	if len(rest) != len(all)-2 || rest[0].ID <= cut {
		t.Fatalf("resume delivered %d events starting at %d, want %d after %d", len(rest), rest[0].ID, len(all)-2, cut)
	}
	if end.Status != "succeeded" {
		t.Fatalf("end = %+v", end)
	}
}

func TestWSTokenQueryParam(t *testing.T) {
	h := newHarness(t)
	h.startEngine()
	a := h.register("ws3@example.com")
	id := h.runOnce(a, h.publishSimple(a))
	c, _, err := h.dialWS("/ws/executions/"+id+"?token="+a.Token, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, end := drain(t, c); end.Status != "succeeded" {
		t.Fatalf("end = %+v", end)
	}
}

func TestWSAuthAndTenancy(t *testing.T) {
	h := newHarness(t)
	a := h.register("wsa@example.com")
	b := h.register("wsb@example.com")
	id := h.runOnce(a, h.publishSimple(a))

	if _, res, err := h.dialWS("/ws/executions/"+id, "", nil); err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("unauthenticated: err=%v res=%v", err, res)
	}
	if _, res, err := h.dialWS("/ws/executions/"+id, b.Token, nil); err == nil || res == nil || res.StatusCode != 404 {
		t.Fatalf("cross-tenant execution: err=%v res=%v", err, res)
	}
	if _, res, err := h.dialWS("/ws/workspaces/"+a.WorkspaceID, b.Token, nil); err == nil || res == nil || res.StatusCode != 404 {
		t.Fatalf("cross-tenant workspace: err=%v res=%v", err, res)
	}
	if _, res, err := h.dialWS("/ws/executions/"+id+"?after=-1", a.Token, nil); err == nil || res == nil || res.StatusCode != 400 {
		t.Fatalf("bad after: err=%v res=%v", err, res)
	}
	if _, res, err := h.dialWS("/ws/executions/00000000-0000-0000-0000-000000000000", a.Token, nil); err == nil || res == nil || res.StatusCode != 404 {
		t.Fatalf("unknown execution: err=%v res=%v", err, res)
	}
}

func TestWSRejectsForeignOrigin(t *testing.T) {
	h := newHarness(t)
	a := h.register("wso@example.com")
	id := h.runOnce(a, h.publishSimple(a))
	hdr := http.Header{"Origin": []string{"https://evil.example"}}
	if _, res, err := h.dialWS("/ws/executions/"+id, a.Token, hdr); err == nil || res == nil || res.StatusCode != 403 {
		t.Fatalf("foreign origin: err=%v res=%v", err, res)
	}
}

func TestWSAllowsConfiguredOrigin(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.AllowedOrigins = []string{"app.example.test"} })
	h.startEngine()
	a := h.register("wsc@example.com")
	id := h.runOnce(a, h.publishSimple(a))
	hdr := http.Header{"Origin": []string{"https://app.example.test"}}
	c, _, err := h.dialWS("/ws/executions/"+id, a.Token, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, end := drain(t, c); end.Status != "succeeded" {
		t.Fatalf("end = %+v", end)
	}
}

func TestWSConnectionCap(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.WSMaxConns = 1 })
	a := h.register("wscap@example.com")
	id := h.runOnce(a, h.publishSimple(a)) // never finishes: no engine
	if _, _, err := h.dialWS("/ws/executions/"+id, a.Token, nil); err != nil {
		t.Fatal(err)
	}
	if _, res, err := h.dialWS("/ws/executions/"+id, a.Token, nil); err == nil || res == nil || res.StatusCode != 503 {
		t.Fatalf("second connection: err=%v res=%v", err, res)
	}
}

func TestWSSlowClientGetsResync(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.WSClientBuffer = 1 })
	a := h.register("wsslow@example.com")
	id := h.runOnce(a, h.publishSimple(a)) // stays running: no engine
	c, _, err := h.dialWS("/ws/executions/"+id, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, c); m.Type != realtime.MsgHello {
		t.Fatalf("hello = %+v", m)
	}
	// Burst far faster than the handler can forward with a one-slot queue.
	for i := 0; i < 20000; i++ {
		h.Srv.Hub.Dispatch([]runtime.Event{{ID: int64(1_000_000 + i), ExecutionID: id, Type: runtime.EvNodeLog}})
		if h.Srv.Hub.Len() == 0 {
			break
		}
	}
	var sawResync bool
	for i := 0; i < 30000 && !sawResync; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var m realtime.Message
		err := wsjson.Read(ctx, c, &m)
		cancel()
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
				t.Fatalf("closed with %v, want 1013", err)
			}
			return
		}
		sawResync = m.Type == realtime.MsgResync
	}
	if !sawResync {
		t.Fatal("no resync message")
	}
}

func TestWSWorkspaceStream(t *testing.T) {
	h := newHarness(t)
	h.startEngine()
	a := h.register("wsw@example.com")
	wf := h.publishSimple(a)
	c, _, err := h.dialWS("/ws/workspaces/"+a.WorkspaceID, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, c); m.Type != realtime.MsgHello {
		t.Fatalf("hello = %+v", m)
	}
	id := h.runOnce(a, wf)
	seen := map[string]bool{}
	for !seen[runtime.EvExecSucceeded] {
		m := readMsg(t, c)
		if m.Type != realtime.MsgEvent || m.Event.ExecutionID != id {
			t.Fatalf("unexpected %+v", m)
		}
		if strings.HasPrefix(m.Event.Type, "node.") {
			t.Fatalf("workspace stream leaked node event %s", m.Event.Type)
		}
		seen[m.Event.Type] = true
	}
	if !seen[runtime.EvExecCreated] {
		t.Fatal("missed execution.created")
	}
}

func TestWSTailRecoversWhenLiveDeliveryIsLost(t *testing.T) {
	// Redis-less deployments in separate processes never see live events;
	// the handler must still finish by tailing the durable log.
	h := newHarness(t)
	a := h.register("wstail@example.com")
	id := h.runOnce(a, h.publishSimple(a))
	h.Srv.wsTail = 50 * time.Millisecond
	c, _, err := h.dialWS("/ws/executions/"+id, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, c); m.Type != realtime.MsgHello {
		t.Fatalf("hello = %+v", m)
	}
	h.Srv.Runtime.OnEvents = nil // events are produced but never delivered live
	h.startEngine()
	_, _, end := drain(t, c)
	if end.Status != "succeeded" {
		t.Fatalf("end = %+v", end)
	}
}

func TestWSWorkspaceTailRecoversLostPushesWithoutDuplicates(t *testing.T) {
	// Without Redis, events committed by other processes never reach this hub;
	// the workspace tail must deliver them, exactly once.
	h := newHarness(t)
	a := h.register("wstail-ws@example.com")
	wf := h.publishSimple(a)
	h.Srv.Runtime.OnEvents = func(evs []runtime.Event) { // deliver only some pushes; the rest must come from the tail
		var some []runtime.Event
		for _, e := range evs {
			if e.Type == runtime.EvExecCreated {
				some = append(some, e)
			}
		}
		h.Srv.Hub.Dispatch(some)
	}
	h.startEngine()
	old := h.runOnce(a, wf) // history that a new stream must not replay
	for i := 0; ; i++ {
		ex, err := h.Srv.Runtime.Get(context.Background(), a.WorkspaceID, old)
		if err == nil && ex.Status == "succeeded" {
			break
		}
		if i > 200 {
			t.Fatal("old execution never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.Srv.wsTail = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Srv.RunWorkspaceTail(ctx)
	time.Sleep(200 * time.Millisecond) // let the tail record where the log ends
	c, _, err := h.dialWS("/ws/workspaces/"+a.WorkspaceID, a.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, c); m.Type != realtime.MsgHello {
		t.Fatalf("hello = %+v", m)
	}
	id := h.runOnce(a, wf)
	counts := map[string]int{}
	for counts[runtime.EvExecSucceeded] == 0 {
		m := readMsg(t, c)
		if m.Type != realtime.MsgEvent {
			t.Fatalf("unexpected %+v", m)
		}
		if m.Event.ExecutionID == old {
			t.Fatalf("replayed history: %s", m.Event.Type)
		}
		if m.Event.ExecutionID == id {
			counts[m.Event.Type]++
		}
	}
	time.Sleep(300 * time.Millisecond) // give duplicates a chance to arrive
	c.SetReadLimit(1 << 20)
	rctx, rcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer rcancel()
	for {
		var m realtime.Message
		if err := wsjson.Read(rctx, c, &m); err != nil {
			break
		}
		if m.Type == realtime.MsgEvent && m.Event.ExecutionID == id {
			counts[m.Event.Type]++
		}
	}
	for typ, n := range counts {
		if n != 1 {
			t.Errorf("%s delivered %d times, want 1", typ, n)
		}
	}
	if counts[runtime.EvExecCreated] != 1 || counts[runtime.EvExecSucceeded] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}
