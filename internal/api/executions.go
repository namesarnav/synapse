package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
)

const maxTriggerBody = 1 << 20

type runBody struct {
	Trigger        any    `json:"trigger"`
	StartNode      string `json:"start_node"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) execID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, r, s.Log, ErrNotFound("execution"))
		return "", false
	}
	return id, true
}

// runtimeErr maps runtime errors to API errors.
func (s *Server) runtimeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		writeError(w, r, s.Log, ErrNotFound("execution"))
	case errors.Is(err, runtime.ErrNotPublished):
		writeError(w, r, s.Log, ErrConflict("workflow is not published"))
	case errors.Is(err, runtime.ErrInvalidTrigger):
		writeError(w, r, s.Log, ErrUnprocessable(err.Error()))
	case errors.Is(err, runtime.ErrTerminal):
		writeError(w, r, s.Log, ErrConflict("execution already finished"))
	case errors.Is(err, runtime.ErrQueueFull):
		w.Header().Set("Retry-After", "5")
		writeError(w, r, s.Log, Err(http.StatusServiceUnavailable, "overloaded", "the execution queue is full; retry later"))
	default:
		writeError(w, r, s.Log, err)
	}
}

// handleRunWorkflow starts a manual execution of the published version.
func (s *Server) handleRunWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	a, _ := userFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	var b runBody
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &b, maxTriggerBody); err != nil {
			writeError(w, r, s.Log, err)
			return
		}
	}
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		b.IdempotencyKey = k
	}
	if len(b.IdempotencyKey) > 200 {
		writeError(w, r, s.Log, ErrUnprocessable("idempotency key is too long"))
		return
	}
	res, err := s.Runtime.Start(r.Context(), runtime.StartParams{
		WorkspaceID: ws, WorkflowID: id, TriggerType: "manual", Trigger: b.Trigger,
		StartNode: b.StartNode, IdempotencyKey: b.IdempotencyKey, CreatedBy: a.User.ID,
	})
	if err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	status := http.StatusAccepted
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"execution": res.Execution, "duplicate": res.Duplicate})
}

func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	p := runtime.ListParams{WorkspaceID: ws, Status: q.Get("status"), WorkflowID: q.Get("workflow_id"), Limit: limit}
	if p.WorkflowID != "" && !uuidRe.MatchString(p.WorkflowID) {
		writeError(w, r, s.Log, ErrBadRequest("invalid workflow_id"))
		return
	}
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		parts := strings.SplitN(string(raw), "|", 2)
		if err != nil || len(parts) != 2 || !uuidRe.MatchString(parts[1]) {
			writeError(w, r, s.Log, ErrBadRequest("invalid cursor"))
			return
		}
		ns, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeError(w, r, s.Log, ErrBadRequest("invalid cursor"))
			return
		}
		t := time.Unix(0, ns).UTC()
		p.AfterTime, p.AfterID = &t, parts[1]
	}
	items, err := s.Runtime.List(r.Context(), p)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	next := ""
	if len(items) == limit {
		last := items[len(items)-1]
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last.CreatedAt.UnixNano(), 10) + "|" + last.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.execID(w, r)
	if !ok {
		return
	}
	ex, err := s.Runtime.Get(r.Context(), ws, id)
	if err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	nodes, err := s.Runtime.Nodes(r.Context(), id)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	attempts, err := s.Runtime.Attempts(r.Context(), id)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	children, err := s.Runtime.Children(r.Context(), id)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution": ex, "nodes": nodes, "attempts": attempts, "children": children})
}

func (s *Server) handleExecutionEvents(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.execID(w, r)
	if !ok {
		return
	}
	if _, err := s.Runtime.Get(r.Context(), ws, id); err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.Runtime.Events(r.Context(), id, after, limit)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

func (s *Server) handleCancelExecution(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.execID(w, r)
	if !ok {
		return
	}
	if _, err := s.Runtime.Get(r.Context(), ws, id); err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	if err := s.Runtime.Cancel(r.Context(), id, "cancelled by user"); err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	ex, err := s.Runtime.Get(r.Context(), ws, id)
	if err != nil {
		s.runtimeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution": ex})
}
