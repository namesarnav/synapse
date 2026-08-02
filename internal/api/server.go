// Package api implements the HTTP API, webhook ingress and WebSocket gateway.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/persistence"
)

// Deps are the collaborators the API needs.
type Deps struct {
	Cfg config.Config
	Log *slog.Logger
	DB  *persistence.DB
	// Extra readiness probes, e.g. redis.
	Ready map[string]func(context.Context) error
}

type Server struct {
	Deps
	mux *http.ServeMux
}

func New(d Deps) *Server {
	s := &Server{Deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health/live", s.handleLive)
	s.mux.HandleFunc("GET /health/ready", s.handleReady)
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
