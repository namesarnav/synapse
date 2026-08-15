package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/namesarnav/synapse/internal/api"
	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/testutil"
)

func TestHealth(t *testing.T) {
	db := testutil.NewDB(t)
	srv := api.New(api.Deps{Cfg: config.Config{MasterKey: make([]byte, 32)}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: db})
	for _, p := range []string{"/health/live", "/health/ready"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, rr.Code, rr.Body)
		}
		if rr.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing request id")
		}
	}
}
