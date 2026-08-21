package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/namesarnav/synapse/internal/api"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/telemetry"
	"github.com/namesarnav/synapse/internal/testutil"
)

func metricsServer(t *testing.T, token string) http.Handler {
	t.Helper()
	db := testutil.NewDB(t)
	m := telemetry.New()
	m.RegisterDB(db)
	srv := api.New(api.Deps{Cfg: config.Config{MasterKey: make([]byte, 32), MetricsToken: token},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: db, Metrics: m})
	return srv.Handler()
}

func TestMetricsEndpointCountsRequestsByRoute(t *testing.T) {
	h := metricsServer(t, "")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health/live", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`synapse_http_requests_total{method="GET",route="GET /health/live",status="2xx"} 1`,
		`synapse_http_requests_total{method="GET",route="unmatched",status="4xx"} 1`,
		`synapse_queue_tasks`, `synapse_executions`, `go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestMetricsTokenRequired(t *testing.T) {
	h := metricsServer(t, "s3cret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rr.Code)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("with token: %d", rr.Code)
	}
}
