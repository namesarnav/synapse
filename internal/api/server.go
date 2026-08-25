// Package api implements the HTTP API, webhook ingress and WebSocket gateway.
package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/ratelimit"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/secrets"
	"github.com/namesarnav/synapse/internal/telemetry"
	"github.com/namesarnav/synapse/internal/tracing"
	"github.com/namesarnav/synapse/internal/triggers"
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
	// Runtime executes workflows; a default one is created when nil.
	Runtime *runtime.Runtime
	// Hub fans events out to WebSocket clients; built when nil. When Runtime is
	// also nil its OnEvents hook is wired to the hub.
	Hub *realtime.Hub
	// Secrets stores workspace secrets; built from Cfg.MasterKey when nil.
	Secrets *secrets.Store
	// Metrics, when set, is served on /metrics and observes every request.
	Metrics *telemetry.Metrics
	// OnPublish runs after a new workflow version is published.
	OnPublish func(ctx context.Context, workspaceID, workflowID string, v wfstore.Version)
}

type Server struct {
	Deps
	Auth        *auth.Store
	Workflows   *wfstore.Store
	Triggers    *triggers.Store
	Hub         *realtime.Hub
	wsConns     atomic.Int64
	wsMax       int
	wsPing      time.Duration
	wsTail      time.Duration
	authLimiter *ratelimit.Limiter
	hookLimiter *ratelimit.Limiter
	mux         *http.ServeMux
}

func New(d Deps) *Server {
	rate := float64(d.Cfg.AuthRatePerMin) / 60
	if d.Cfg.AuthRatePerMin <= 0 {
		rate, d.Cfg.AuthRatePerMin = 20.0/60, 20
	}
	if d.Secrets == nil {
		st, err := secrets.New(d.DB, d.Cfg.MasterKey)
		if err != nil {
			panic("api: " + err.Error())
		}
		d.Secrets = st
	}
	if d.Hub == nil {
		d.Hub = realtime.NewHub(d.Cfg.WSClientBuffer)
	}
	if d.Runtime == nil {
		d.Runtime = runtime.New(&runtime.Runtime{DB: d.DB, Log: d.Log, MaxQueueDepth: d.Cfg.MaxQueueDepth,
			MaxDepth: d.Cfg.MaxSubWorkflowDepth, LeaseDuration: d.Cfg.LeaseDuration, MaxDeliveries: d.Cfg.MaxDeliveries,
			Secrets: d.Secrets, OnEvents: d.Hub.Dispatch})
	}
	wsMax := d.Cfg.WSMaxConns
	if wsMax <= 0 {
		wsMax = 5000
	}
	hookRate, hookBurst := d.Cfg.WebhookRatePerSec, d.Cfg.WebhookRateBurst
	if hookRate <= 0 {
		hookRate = 50
	}
	if hookBurst <= 0 {
		hookBurst = 100
	}
	if d.Cfg.WebhookMaxBodyBytes <= 0 {
		d.Cfg.WebhookMaxBodyBytes = 1 << 20
	}
	s := &Server{
		Deps:        d,
		Auth:        &auth.Store{DB: d.DB, TTL: d.Cfg.SessionTTL},
		Workflows:   &wfstore.Store{DB: d.DB},
		authLimiter: ratelimit.New(rate, d.Cfg.AuthRatePerMin),
		hookLimiter: ratelimit.New(hookRate, hookBurst),
		Hub:         d.Hub,
		wsMax:       wsMax,
		wsPing:      20 * time.Second,
		wsTail:      2 * time.Second,
		Triggers:    &triggers.Store{DB: d.DB, RT: d.Runtime, Log: d.Log},
		mux:         http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health/live", s.handleLive)
	s.mux.HandleFunc("GET /health/ready", s.handleReady)
	if s.Metrics != nil {
		s.mux.Handle("GET /metrics", s.metricsAuth(s.Metrics.Handler()))
	}

	s.mux.HandleFunc("POST /hooks/{endpoint_id}", s.handleWebhook)

	s.mux.HandleFunc("GET /ws/executions/{id}", withQueryToken(s.requireAuth(s.handleWSExecution)))
	s.mux.HandleFunc("GET /ws/workspaces/{ws}", withQueryToken(s.requireWorkspace(auth.RoleViewer, s.handleWSWorkspace)))

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
	s.mux.HandleFunc("POST "+ws+"/workflows/{id}/run", s.requireWorkspace(auth.RoleMember, s.handleRunWorkflow))
	s.mux.HandleFunc("GET "+ws+"/workflows/{id}/webhooks", s.requireWorkspace(auth.RoleViewer, s.handleListWebhooks))
	s.mux.HandleFunc("GET "+ws+"/secrets", s.requireWorkspace(auth.RoleMember, s.handleListSecrets))
	s.mux.HandleFunc("PUT "+ws+"/secrets/{name}", s.requireWorkspace(auth.RoleAdmin, s.handlePutSecret))
	s.mux.HandleFunc("DELETE "+ws+"/secrets/{name}", s.requireWorkspace(auth.RoleAdmin, s.handleDeleteSecret))
	s.mux.HandleFunc("GET "+ws+"/executions", s.requireWorkspace(auth.RoleViewer, s.handleListExecutions))
	s.mux.HandleFunc("GET "+ws+"/executions/{id}", s.requireWorkspace(auth.RoleViewer, s.handleGetExecution))
	s.mux.HandleFunc("GET "+ws+"/executions/{id}/events", s.requireWorkspace(auth.RoleViewer, s.handleExecutionEvents))
	s.mux.HandleFunc("POST "+ws+"/executions/{id}/replay", s.requireWorkspace(auth.RoleMember, s.handleReplayExecution))
	s.mux.HandleFunc("POST "+ws+"/executions/{id}/replay/{node}", s.requireWorkspace(auth.RoleMember, s.handleReplayExecution))
	s.mux.HandleFunc("POST "+ws+"/executions/{id}/cancel", s.requireWorkspace(auth.RoleMember, s.handleCancelExecution))
}

func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	var observe func(string, string, int, time.Duration)
	if s.Metrics != nil {
		observe = s.Metrics.ObserveHTTP
	}
	h = withAccessLog(s.Log, observe)(h)
	h = withRecover(s.Log)(h)
	h = withRequestID(h)
	return tracing.HTTP(h)
}

// metricsAuth requires the configured bearer token, when there is one.
func (s *Server) metricsAuth(next http.Handler) http.Handler {
	tok := s.Cfg.MetricsToken
	if tok == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			writeError(w, r, s.Log, ErrUnauthorized())
			return
		}
		next.ServeHTTP(w, r)
	})
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
