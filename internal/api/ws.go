package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
)

const (
	wsWriteTimeout = 5 * time.Second
	wsReplayBatch  = 500
)

// withQueryToken lets non-browser clients pass ?token= since they cannot always set headers.
func withQueryToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("token") != "" && r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+q.Get("token"))
		}
		next(w, r)
	}
}

func (s *Server) wsAccept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	if s.wsConns.Add(1) > int64(s.wsMax) {
		s.wsConns.Add(-1)
		w.Header().Set("Retry-After", "5")
		writeError(w, r, s.Log, Err(http.StatusServiceUnavailable, "overloaded", "too many WebSocket connections"))
		return nil, false
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.Cfg.AllowedOrigins})
	if err != nil {
		s.wsConns.Add(-1) // Accept already wrote the error response
		return nil, false
	}
	c.SetReadLimit(4 << 10)
	return c, true
}

// handleWSExecution streams one execution's events: replay from ?after=<id>,
// then live. Slow clients are dropped with 1013 and resume via ?after=.
func (s *Server) handleWSExecution(w http.ResponseWriter, r *http.Request) {
	a, _ := userFrom(r.Context())
	id := r.PathValue("id")
	wsID, err := s.Runtime.WorkspaceOf(r.Context(), id)
	if err == nil {
		_, err = s.Auth.RoleIn(r.Context(), a.User.ID, wsID)
	}
	if errors.Is(err, persistence.ErrNotFound) {
		writeError(w, r, s.Log, ErrNotFound("execution"))
		return
	}
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			writeError(w, r, s.Log, ErrBadRequest("after must be a non-negative event id"))
			return
		}
	}
	c, ok := s.wsAccept(w, r)
	if !ok {
		return
	}
	defer s.wsConns.Add(-1)
	s.streamExecution(r.Context(), c, wsID, id, after)
}

func (s *Server) streamExecution(ctx context.Context, c *websocket.Conn, wsID, id string, after int64) {
	defer c.CloseNow()
	ctx = c.CloseRead(ctx)
	// Subscribe before replaying so nothing published in between is missed.
	sub := s.Hub.Execution(id)
	defer s.Hub.Unsubscribe(sub)

	ex, err := s.Runtime.Get(ctx, wsID, id)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "lookup failed")
		return
	}
	send := func(m realtime.Message) error {
		wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return wsjson.Write(wctx, c, m)
	}
	if send(realtime.Message{Type: realtime.MsgHello, ExecutionID: id, Status: string(ex.Status), LastEventID: after}) != nil {
		return
	}
	last := after
	finished := false
	// forward sends one event if it is new; it returns false when the stream should stop.
	forward := func(e runtime.Event) bool {
		if e.ID <= last {
			return true
		}
		last = e.ID
		if send(realtime.Message{Type: realtime.MsgEvent, Event: &e}) != nil {
			return false
		}
		if e.ExecutionID == id && realtime.IsTerminal(e.Type) {
			finished = true
		}
		return true
	}
	// catchUp reads the durable log; it also covers messages Redis dropped.
	catchUp := func() bool {
		for {
			evs, err := s.Runtime.Events(ctx, id, last, wsReplayBatch)
			if err != nil {
				// Transient DB error: the next tail tick retries.
				return ctx.Err() == nil
			}
			for _, e := range evs {
				if !forward(e) {
					return false
				}
			}
			if len(evs) < wsReplayBatch {
				return true
			}
		}
	}
	end := func() {
		status := ex.Status
		if cur, err := s.Runtime.Get(ctx, wsID, id); err == nil {
			status = cur.Status
		}
		_ = send(realtime.Message{Type: realtime.MsgEnd, ExecutionID: id, Status: string(status), LastEventID: last})
		_ = c.Close(websocket.StatusNormalClosure, "execution finished")
	}
	if !catchUp() {
		return
	}
	if finished {
		end()
		return
	}
	ping := time.NewTicker(s.wsPing)
	defer ping.Stop()
	tail := time.NewTicker(s.wsTail)
	defer tail.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub.C:
			if !forward(e) {
				return
			}
		case <-sub.Overflow:
			_ = send(realtime.Message{Type: realtime.MsgResync, LastEventID: last, Message: "client too slow; reconnect with after=last_event_id"})
			_ = c.Close(websocket.StatusTryAgainLater, "slow consumer")
			return
		case <-tail.C:
			if !catchUp() {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
		if finished {
			// Flush anything already committed before closing.
			if catchUp() {
				end()
			}
			return
		}
	}
}

// handleWSWorkspace streams execution-level events for a whole workspace so
// list views update live. It has no replay: clients refetch after a resync.
func (s *Server) handleWSWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	c, ok := s.wsAccept(w, r)
	if !ok {
		return
	}
	defer s.wsConns.Add(-1)
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	sub := s.Hub.Workspace(ws)
	defer s.Hub.Unsubscribe(sub)
	send := func(m realtime.Message) error {
		wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return wsjson.Write(wctx, c, m)
	}
	if send(realtime.Message{Type: realtime.MsgHello}) != nil {
		return
	}
	ping := time.NewTicker(s.wsPing)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub.C:
			if send(realtime.Message{Type: realtime.MsgEvent, Event: &e}) != nil {
				return
			}
		case <-sub.Overflow:
			_ = send(realtime.Message{Type: realtime.MsgResync, Message: "client too slow; refetch and reconnect"})
			_ = c.Close(websocket.StatusTryAgainLater, "slow consumer")
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// tailWindow is how far below its high-water mark the workspace tail re-reads,
// to catch events whose ids committed out of order.
const tailWindow = 5000

// RunWorkspaceTail feeds the workspace stream from the event log so it stays
// complete when Redis is absent or drops a message. Pushes still win on latency;
// the hub deduplicates by event id. It only queries while a workspace stream is open.
func (s *Server) RunWorkspaceTail(ctx context.Context) {
	t := time.NewTicker(s.wsTail)
	defer t.Stop()
	var high int64
	primed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.Hub.WorkspaceSubscribers() == 0 {
			// Remember where the log ends so a new stream does not replay history.
			if id, err := s.Runtime.MaxEventID(ctx); err == nil {
				high = id
			}
			primed = false
			continue
		}
		from := high - tailWindow
		if from < 0 {
			from = 0
		}
		evs, err := s.Runtime.ExecutionEventsSince(ctx, from, 2000)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Warn("workspace tail", "err", err)
			}
			continue
		}
		fresh := evs[:0:0]
		for _, e := range evs {
			if e.ID <= high && !primed {
				s.Hub.MarkSeen(e.ID)
				continue
			}
			fresh = append(fresh, e)
		}
		s.Hub.DispatchWorkspace(fresh)
		for _, e := range evs {
			if e.ID > high {
				high = e.ID
			}
		}
		primed = true
	}
}
