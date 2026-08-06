// Package api implements the HTTP API, webhook ingress and WebSocket gateway.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/ratelimit"
	"github.com/namesarnav/synapse/internal/workflow"
	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

// Deps are the collaborators the API needs.
type Deps struct {
	Cfg config.Config
	Log *slog.Logger
	DB  *persistence.DB
	// Extra readiness probes, e.g. redis.
	Ready map[string]func(context.Context) error
	// Checker validates expressions inside workflow graphs (nil skips it).
	Checker workflow.ExprChecker
	// OnPublish runs after a new workflow version is published.
	OnPublish func(ctx context.Context, workspaceID, workflowID string, v wfstore.Version)
}

type Server struct {
	Deps
	Auth        *auth.Store
	Workflows   *wfstore.Store
	authLimiter *ratelimit.Limiter
	mux         *http.ServeMux
}

func New(d Deps) *Server {
	rate := float64(d.Cfg.AuthRatePerMin) / 60
	if d.Cfg.AuthRatePerMin <= 0 {
		rate, d.Cfg.AuthRatePerMin = 20.0/60, 20
	}
	s := &Server{
		Deps:        d,
		Auth:        &auth.Store{DB: d.DB},
		Workflows:   &wfstore.Store{DB: d.DB},
		authLimiter: ratelimit.New(rate, d.Cfg.AuthRatePerMin),
		mux:         http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health/live", s.handleLive)
	s.mux.HandleFunc("GET /health/ready", s.handleReady)

	const v1 = "/api/v1"
	s.mux.HandleFunc("POST "+v1+"/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST "+v1+"/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST "+v1+"/auth/logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("POST "+v1+"/auth/refresh", s.requireAuth(s.handleRefresh))
	s.mux.HandleFunc("GET "+v1+"/auth/me", s.requireAuth(s.handleMe))
	s.mux.HandleFunc("POST "+v1+"/workspaces", s.requireAuth(s.handleCreateWorkspace))
	s.mux.HandleFunc("GET "+v1+"/node-types", s.requireAuth(s.handleNodeTypes))

	ws := v1 + "/workspaces/{ws}"
	s.mux.HandleFunc("POST "+ws+"/members", s.requireWorkspace(auth.RoleAdmin, s.handleAddMember))
	s.mux.HandleFunc("GET "+ws+"/workflows", s.requireWorkspace(auth.RoleViewer, s.handleListWorkflows))
	s.mux.HandleFunc("POST "+ws+"/workflows", s.requireWorkspace(auth.RoleMember, s.handleCreateWorkflow))
	s.mux.HandleFunc("GET "+ws+"/workflows/{id}", s.requireWorkspace(auth.RoleViewer, s.handleGetWorkflow))
	s.mux.HandleFunc("PUT "+ws+"/workflows/{id}", s.requireWorkspace(auth.RoleMember, s.handleUpdateWorkflow))
	s.mux.HandleFunc("DELETE "+ws+"/workflows/{id}", s.requireWorkspace(auth.RoleMember, s.handleDeleteWorkflow))
	s.mux.HandleFunc("POST "+ws+"/workflows/{id}/validate", s.requireWorkspace(auth.RoleViewer, s.handleValidateWorkflow))
	s.mux.HandleFunc("POST "+ws+"/workflows/{id}/publish", s.requireWorkspace(auth.RoleMember, s.handlePublishWorkflow))
	s.mux.HandleFunc("POST "+ws+"/workflows/{id}/unpublish", s.requireWorkspace(auth.RoleMember, s.handleUnpublishWorkflow))
	s.mux.HandleFunc("GET "+ws+"/workflows/{id}/versions", s.requireWorkspace(auth.RoleViewer, s.handleListVersions))
	s.mux.HandleFunc("GET "+ws+"/workflows/{id}/versions/{version}", s.requireWorkspace(auth.RoleViewer, s.handleGetVersion))
}

func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = withAccessLog(s.Log, nil)(h)
	h = withRecover(s.Log)(h)
	h = withRequestID(h)
	return h
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{}
	ok := true
	if err := s.DB.Ping(ctx); err != nil {
		checks["postgres"] = err.Error()
		ok = false
	} else {
		checks["postgres"] = "ok"
	}
	for name, fn := range s.Ready {
		if err := fn(ctx); err != nil {
			checks[name] = err.Error()
			ok = false
		} else {
			checks[name] = "ok"
		}
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "unavailable"}[ok], "checks": checks})
}
