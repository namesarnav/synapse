package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/namesarnav/synapse/internal/config"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/testutil"
)

type harness struct {
	t   *testing.T
	DB  *persistence.DB
	Srv *Server
	TS  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testutil.NewDB(t)
	cfg := config.Config{AuthRatePerMin: 100000}
	srv := New(Deps{Cfg: cfg, Log: logging.New("test", "error", io.Discard, nil), DB: db, Checker: expressions.Checker{}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, DB: db, Srv: srv, TS: ts}
}

type resp struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r resp) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

func (r resp) errCode() string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

// do performs a bearer-authenticated request (token may be empty).
func (h *harness) do(method, path, token string, body any) resp {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.TS.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Body: b, Header: res.Header}
}

type account struct {
	Token       string
	UserID      string
	WorkspaceID string
	Email       string
}

func (h *harness) register(email string) account {
	h.t.Helper()
	r := h.do("POST", "/api/v1/auth/register", "", map[string]any{"email": email, "password": "correct-horse-battery", "display_name": "T"})
	if r.Status != http.StatusCreated {
		h.t.Fatalf("register %s: %d %s", email, r.Status, r.Body)
	}
	var s sessionResp
	r.JSON(h.t, &s)
	return account{Token: s.Token, UserID: s.User.ID, WorkspaceID: s.Workspaces[0].ID, Email: email}
}

func (a account) wf(path string) string {
	return fmt.Sprintf("/api/v1/workspaces/%s/workflows%s", a.WorkspaceID, path)
}

var simpleGraph = map[string]any{
	"nodes": []map[string]any{
		{"id": "t", "type": "manual_trigger"},
		{"id": "log", "type": "log", "config": map[string]any{"message": "hello"}},
	},
	"edges": []map[string]any{{"source": "t", "target": "log"}},
}
