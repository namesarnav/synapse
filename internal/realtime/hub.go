// Package realtime fans execution events out to WebSocket clients. Postgres
// stays the source of truth: the hub and Redis only cut latency, and clients
// that fall behind are dropped and resynchronise from the event log.
package realtime

import (
	"sync"
	"sync/atomic"

	"github.com/namesarnav/synapse/internal/runtime"
)

// Sub is one subscriber's bounded queue.
type Sub struct {
	C chan runtime.Event
	// Overflow is closed when the subscriber fell behind and was dropped.
	Overflow chan struct{}

	overflowed atomic.Bool
	key        string
	scope      scope
}

type scope int

const (
	scopeExec scope = iota
	scopeWorkspace
)

// Metrics receives hub observations; all fields are optional.
type Metrics struct {
	Dropped func()
	Clients func(delta int)
}

// Hub routes events to subscribers without ever blocking the publisher.
type Hub struct {
	Buffer  int
	Metrics Metrics

	mu     sync.RWMutex
	byExec map[string]map[*Sub]struct{}
	byWS   map[string]map[*Sub]struct{}
}

// NewHub returns a hub whose subscribers buffer up to buffer events.
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = 256
	}
	return &Hub{Buffer: buffer, byExec: map[string]map[*Sub]struct{}{}, byWS: map[string]map[*Sub]struct{}{}}
}

// Execution subscribes to every event of one execution.
func (h *Hub) Execution(id string) *Sub { return h.add(scopeExec, id) }

// Workspace subscribes to execution-level events of a workspace.
func (h *Hub) Workspace(id string) *Sub { return h.add(scopeWorkspace, id) }

func (h *Hub) add(sc scope, key string) *Sub {
	s := &Sub{C: make(chan runtime.Event, h.Buffer), Overflow: make(chan struct{}), key: key, scope: sc}
	h.mu.Lock()
	m := h.index(sc)
	if m[key] == nil {
		m[key] = map[*Sub]struct{}{}
	}
	m[key][s] = struct{}{}
	h.mu.Unlock()
	if h.Metrics.Clients != nil {
		h.Metrics.Clients(1)
	}
	return s
}

func (h *Hub) index(sc scope) map[string]map[*Sub]struct{} {
	if sc == scopeWorkspace {
		return h.byWS
	}
	return h.byExec
}

// Unsubscribe removes a subscriber; it is safe to call more than once.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	removed := h.remove(s)
	h.mu.Unlock()
	if removed && h.Metrics.Clients != nil {
		h.Metrics.Clients(-1)
	}
}

func (h *Hub) remove(s *Sub) bool {
	m := h.index(s.scope)
	set, ok := m[s.key]
	if !ok {
		return false
	}
	if _, ok := set[s]; !ok {
		return false
	}
	delete(set, s)
	if len(set) == 0 {
		delete(m, s.key)
	}
	return true
}

// Dispatch delivers events to matching subscribers. It never blocks: a
// subscriber with a full queue is dropped and told to resynchronise.
func (h *Hub) Dispatch(evs []runtime.Event) {
	for i := range evs {
		e := evs[i]
		h.mu.RLock()
		var overflowed []*Sub
		for s := range h.byExec[e.ExecutionID] {
			if !h.offer(s, e) {
				overflowed = append(overflowed, s)
			}
		}
		if e.WorkspaceID != "" && isExecutionLevel(e.Type) {
			for s := range h.byWS[e.WorkspaceID] {
				if !h.offer(s, e) {
					overflowed = append(overflowed, s)
				}
			}
		}
		h.mu.RUnlock()
		for _, s := range overflowed {
			h.drop(s)
		}
	}
}

func (h *Hub) offer(s *Sub, e runtime.Event) bool {
	if s.overflowed.Load() {
		return true
	}
	select {
	case s.C <- e:
		return true
	default:
		return false
	}
}

func (h *Hub) drop(s *Sub) {
	if s.overflowed.CompareAndSwap(false, true) {
		close(s.Overflow)
		if h.Metrics.Dropped != nil {
			h.Metrics.Dropped()
		}
	}
	h.Unsubscribe(s)
}

// Len returns the number of live subscribers.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, m := range []map[string]map[*Sub]struct{}{h.byExec, h.byWS} {
		for _, set := range m {
			n += len(set)
		}
	}
	return n
}

func isExecutionLevel(t string) bool {
	switch t {
	case runtime.EvExecCreated, runtime.EvExecStarted, runtime.EvExecSucceeded, runtime.EvExecFailed,
		runtime.EvExecCancelled, runtime.EvExecCancelReq:
		return true
	}
	return false
}
