package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

func input(t *testing.T, typ workflow.NodeType, cfg any, vars map[string]any) Input {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	n := &workflow.Node{ID: "n", Type: typ, Config: raw}
	c, err := workflow.DecodeConfig(n)
	if err != nil {
		t.Fatal(err)
	}
	return Input{Node: n, Config: c, Env: &expressions.Env{Vars: vars}, Attempt: 1, IdempotencyKey: "exec:n:1", Log: slog.New(slog.DiscardHandler)}
}

func nodeErr(t *testing.T, err error) *engine.NodeError {
	t.Helper()
	var ne *engine.NodeError
	if !errors.As(err, &ne) {
		t.Fatalf("error %v is not a NodeError", err)
	}
	return ne
}

func TestHTTPRequestSuccess(t *testing.T) {
	var gotBody, gotIdem, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotIdem, gotAuth, gotQuery = string(b), r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "a=b")
		_, _ = w.Write([]byte(`{"ok":true,"n":2}`))
	}))
	defer srv.Close()
	ex := NewHTTPExecutor(HTTPOptions{AllowPrivate: true})
	in := input(t, workflow.TypeHTTPRequest, map[string]any{
		"method": "POST", "url": srv.URL + "/x/{{ trigger.id }}",
		"headers": map[string]any{"Authorization": "Bearer {{ trigger.tok }}"},
		"query":   map[string]any{"q": "{{ trigger.id }}"},
		"body":    map[string]any{"name": "{{ trigger.id }}", "n": "{{ 1 + 1 }}"}, "idempotency_header": "Idempotency-Key",
	}, map[string]any{"trigger": map[string]any{"id": "42", "tok": "t0k"}})
	out, err := ex.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["status"] != 200.0 || m["body"].(map[string]any)["n"] != 2.0 {
		t.Fatalf("out = %#v", m)
	}
	if _, ok := m["headers"].(map[string]any)["set-cookie"]; ok {
		t.Error("set-cookie leaked into output")
	}
	if gotIdem != "exec:n:1" || gotAuth != "Bearer t0k" || gotQuery != "q=42" {
		t.Errorf("idem=%q auth=%q query=%q", gotIdem, gotAuth, gotQuery)
	}
	if !strings.Contains(gotBody, `"name":"42"`) || !strings.Contains(gotBody, `"n":2`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	for code, retry := range map[int]bool{400: false, 404: false, 429: true, 500: true, 503: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		out, err := NewHTTPExecutor(HTTPOptions{AllowPrivate: true}).Execute(context.Background(),
			input(t, workflow.TypeHTTPRequest, map[string]any{"url": srv.URL}, nil))
		srv.Close()
		ne := nodeErr(t, err)
		if ne.Retryable != retry || ne.Code != engine.CodeHTTP {
			t.Errorf("status %d: %+v", code, ne)
		}
		if out == nil {
			t.Errorf("status %d: response should still be returned for debugging", code)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	if _, err := NewHTTPExecutor(HTTPOptions{AllowPrivate: true}).Execute(context.Background(),
		input(t, workflow.TypeHTTPRequest, map[string]any{"url": srv.URL, "expect_status": []int{404}}, nil)); err != nil {
		t.Errorf("expected status 404 rejected: %v", err)
	}
}

func TestHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer srv.Close()
	in := input(t, workflow.TypeHTTPRequest, map[string]any{"url": srv.URL}, nil)
	in.Node.TimeoutMS = 50
	_, err := NewHTTPExecutor(HTTPOptions{AllowPrivate: true}).Execute(context.Background(), in)
	ne := nodeErr(t, err)
	if ne.Code != engine.CodeTimeout || !ne.Retryable {
		t.Errorf("err = %+v", ne)
	}
}

func TestHTTPBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request reached a loopback server") }))
	defer srv.Close()
	ex := NewHTTPExecutor(HTTPOptions{})
	for _, u := range []string{srv.URL, "http://127.0.0.1:1/", "http://[::1]:1/", "http://169.254.169.254/latest/meta-data", "http://10.0.0.1/", "http://192.168.1.1/", "http://0.0.0.0/", "http://localhost:1/"} {
		_, err := ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest, map[string]any{"url": u}, nil))
		ne := nodeErr(t, err)
		if ne.Code != engine.CodeConfig || ne.Retryable {
			t.Errorf("%s: %+v", u, ne)
		}
	}
}

func TestHTTPRedirectsNotFollowedByDefault(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("final")) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redir.Close()
	ex := NewHTTPExecutor(HTTPOptions{AllowPrivate: true})
	_, err := ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest, map[string]any{"url": redir.URL}, nil))
	if ne := nodeErr(t, err); !strings.Contains(ne.Message, "302") {
		t.Errorf("redirect response should surface as a non-2xx status: %+v", ne)
	}
	out, err := ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest, map[string]any{"url": redir.URL, "follow_redirects": true}, nil))
	if err != nil || out.(map[string]any)["body"] != "final" {
		t.Errorf("follow_redirects: %v %v", out, err)
	}
}

func TestHTTPRejectsHeaderInjectionAndOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()
	ex := NewHTTPExecutor(HTTPOptions{AllowPrivate: true, MaxBodyBytes: 1024})
	_, err := ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest, map[string]any{"url": srv.URL}, nil))
	if ne := nodeErr(t, err); ne.Retryable {
		t.Errorf("oversize body: %+v", ne)
	}
	_, err = ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest,
		map[string]any{"url": srv.URL, "headers": map[string]any{"X-A": "{{ trigger.v }}"}}, map[string]any{"trigger": map[string]any{"v": "a\r\nX-Evil: 1"}}))
	if ne := nodeErr(t, err); ne.Code != engine.CodeConfig {
		t.Errorf("header injection: %+v", ne)
	}
}

func TestBlockedIPRanges(t *testing.T) {
	for s, want := range map[string]bool{"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.0.5": true, "169.254.169.254": true,
		"::1": true, "fe80::1": true, "fc00::1": true, "100.64.0.1": true, "::ffff:127.0.0.1": true, "0.0.0.0": true,
		"0.1.2.3": true, "255.255.255.255": true, "240.0.0.1": true, "198.18.0.1": true, "192.0.0.8": true,
		"64:ff9b::7f00:1": true, "2002:7f00:1::1": true, "::": true, "ff02::1": true, "192.0.2.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false} {
		if got := blockedIP(netip.MustParseAddr(s)); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestTransformAndLogAndEmail(t *testing.T) {
	vars := map[string]any{"trigger": map[string]any{"n": 2.0, "who": "Ada"}}
	reg := NewRegistry(Options{})
	out, err := reg[workflow.TypeTransform].Execute(context.Background(), input(t, workflow.TypeTransform, map[string]any{"expression": "trigger.n * 5"}, vars))
	if err != nil || out != 10.0 {
		t.Errorf("expression transform: %v %v", out, err)
	}
	out, err = reg[workflow.TypeTransform].Execute(context.Background(), input(t, workflow.TypeTransform,
		map[string]any{"output": map[string]any{"greeting": "hi {{ trigger.who }}", "n": "{{ trigger.n }}"}}, vars))
	if err != nil || out.(map[string]any)["greeting"] != "hi Ada" || out.(map[string]any)["n"] != 2.0 {
		t.Errorf("output transform: %v %v", out, err)
	}
	if _, err = reg[workflow.TypeTransform].Execute(context.Background(), input(t, workflow.TypeTransform, map[string]any{"expression": "1 +"}, vars)); nodeErr(t, err).Code != engine.CodeExpression {
		t.Error("bad expression code")
	}
	out, err = reg[workflow.TypeLog].Execute(context.Background(), input(t, workflow.TypeLog, map[string]any{"message": "hello {{ trigger.who }}", "level": "warn"}, vars))
	if err != nil || out.(map[string]any)["message"] != "hello Ada" {
		t.Errorf("log: %v %v", out, err)
	}
	mailer := NewMockMailer()
	reg = NewRegistry(Options{Mailer: mailer})
	out, err = reg[workflow.TypeEmail].Execute(context.Background(), input(t, workflow.TypeEmail, map[string]any{"to": "a@b.c", "subject": "Hi {{ trigger.who }}", "body": "x"}, vars))
	if err != nil || len(mailer.Sent()) != 1 || mailer.Sent()[0].Subject != "Hi Ada" || out.(map[string]any)["provider"] != "mock" {
		t.Errorf("email: %v %v %v", out, err, mailer.Sent())
	}
}

func TestSSRFGuardBlocksLoopbackByDefault(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{} }))
	defer srv.Close()
	ex := NewHTTPExecutor(HTTPOptions{})
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), strings.Replace(srv.URL, "127.0.0.1", "[::1]", 1),
		strings.Replace(srv.URL, "127.0.0.1", "2130706433", 1), strings.Replace(srv.URL, "127.0.0.1", "0x7f.1", 1)} {
		_, err := ex.Execute(context.Background(), input(t, workflow.TypeHTTPRequest, map[string]any{"url": u}, nil))
		if err == nil {
			t.Errorf("%s: expected an error", u)
		}
	}
	select {
	case <-hit:
		t.Fatal("the guard let a request reach a loopback server")
	default:
	}
}
