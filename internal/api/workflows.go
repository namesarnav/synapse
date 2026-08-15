package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

const maxWorkflowBody = 4 << 20

func itoa(n int) string { return strconv.Itoa(n) }

func (s *Server) handleNodeTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"node_types": workflow.Catalog()})
}

func (s *Server) validateGraph(g *workflow.Graph) *workflow.Report {
	g.Normalize()
	return workflow.Validate(g, s.Checker)
}

func (s *Server) wfID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, r, s.Log, ErrNotFound("workflow"))
		return "", false
	}
	return id, true
}

func (s *Server) wfErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		writeError(w, r, s.Log, ErrNotFound("workflow"))
	case errors.Is(err, wfstore.ErrStaleRevision):
		writeError(w, r, s.Log, ErrConflict("workflow was modified by someone else; reload and retry"))
	default:
		writeError(w, r, s.Log, err)
	}
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var after *time.Time
	var afterID string
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
		after, afterID = &t, parts[1]
	}
	items, created, err := s.Workflows.List(r.Context(), ws, q.Get("q"), limit, after, afterID)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	next := ""
	if len(items) == limit {
		last := len(items) - 1
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(created[last].UnixNano(), 10) + "|" + items[last].ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

type wfBody struct {
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Graph       *workflow.Graph `json:"graph"`
	Revision    int             `json:"revision"`
}

func checkName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return ErrUnprocessable("name must be 1-200 characters")
	}
	return nil
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	a, _ := userFrom(r.Context())
	var b wfBody
	if err := decodeJSON(r, &b, maxWorkflowBody); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	if b.Name == nil {
		writeError(w, r, s.Log, ErrUnprocessable("name is required"))
		return
	}
	if err := checkName(*b.Name); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	g := workflow.Graph{}
	if b.Graph != nil {
		g = *b.Graph
	}
	if len(g.Nodes) > workflow.MaxNodes || len(g.Edges) > workflow.MaxEdges {
		writeError(w, r, s.Log, ErrUnprocessable("graph is too large"))
		return
	}
	desc := ""
	if b.Description != nil {
		desc = *b.Description
	}
	wf, err := s.Workflows.Create(r.Context(), ws, a.User.ID, strings.TrimSpace(*b.Name), desc, g)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusCreated, wf)
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	wf, err := s.Workflows.Get(r.Context(), ws, id)
	if err != nil {
		s.wfErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	var b wfBody
	if err := decodeJSON(r, &b, maxWorkflowBody); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	if b.Name != nil {
		if err := checkName(*b.Name); err != nil {
			writeError(w, r, s.Log, err)
			return
		}
		t := strings.TrimSpace(*b.Name)
		b.Name = &t
	}
	if b.Graph != nil && (len(b.Graph.Nodes) > workflow.MaxNodes || len(b.Graph.Edges) > workflow.MaxEdges) {
		writeError(w, r, s.Log, ErrUnprocessable("graph is too large"))
		return
	}
	wf, err := s.Workflows.Update(r.Context(), ws, id, wfstore.Update{Name: b.Name, Description: b.Description, Graph: b.Graph, Revision: b.Revision})
	if err != nil {
		s.wfErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	if err := s.Workflows.Delete(r.Context(), ws, id); err != nil {
		s.wfErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleValidate validates a supplied graph, or the stored draft when no body is sent.
func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	var b wfBody
	var g workflow.Graph
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &b, maxWorkflowBody); err != nil {
			writeError(w, r, s.Log, err)
			return
		}
	}
	if b.Graph != nil {
		if _, err := s.Workflows.Get(r.Context(), ws, id); err != nil {
			s.wfErr(w, r, err)
			return
		}
		g = *b.Graph
	} else {
		wf, err := s.Workflows.Get(r.Context(), ws, id)
		if err != nil {
			s.wfErr(w, r, err)
			return
		}
		g = wf.Graph
	}
	rep := s.validateGraph(&g)
	writeJSON(w, http.StatusOK, map[string]any{"valid": rep.Valid(), "issues": rep.Issues})
}

func (s *Server) handlePublishWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	a, _ := userFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	var b struct {
		Notes string `json:"notes"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &b, 8<<10); err != nil {
			writeError(w, r, s.Log, err)
			return
		}
	}
	v, created, err := s.Workflows.Publish(r.Context(), ws, id, a.User.ID, b.Notes, func(g workflow.Graph) error {
		if rep := s.validateGraph(&g); !rep.Valid() {
			return ErrUnprocessable("workflow is invalid").With(map[string]any{"issues": rep.Issues})
		}
		return nil
	})
	if err != nil {
		s.wfErr(w, r, err)
		return
	}
	if err := s.Triggers.Sync(r.Context(), ws, id); err != nil {
		s.Log.Error("trigger sync failed", "workflow", id, "err", err)
		writeError(w, r, s.Log, err)
		return
	}
	if created && s.OnPublish != nil {
		s.OnPublish(r.Context(), ws, id, v)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, v)
}

func (s *Server) handleUnpublishWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	if err := s.Workflows.Unpublish(r.Context(), ws, id); err != nil {
		s.wfErr(w, r, err)
		return
	}
	if err := s.Triggers.Sync(r.Context(), ws, id); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	vs, err := s.Workflows.Versions(r.Context(), ws, id)
	if err != nil {
		s.wfErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": vs})
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || n < 1 {
		writeError(w, r, s.Log, ErrNotFound("version"))
		return
	}
	v, err := s.Workflows.GetVersion(r.Context(), ws, id, n)
	if errors.Is(err, persistence.ErrNotFound) {
		writeError(w, r, s.Log, ErrNotFound("version"))
		return
	}
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
