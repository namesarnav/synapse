package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRegisterLoginLogout(t *testing.T) {
	h := newHarness(t)
	a := h.register("Alice@Example.com")

	if r := h.do("POST", "/api/v1/auth/register", "", map[string]any{"email": "alice@example.com", "password": "correct-horse-battery"}); r.Status != 409 {
		t.Fatalf("duplicate email (case-insensitive) got %d", r.Status)
	}
	if r := h.do("POST", "/api/v1/auth/register", "", map[string]any{"email": "bob@example.com", "password": "short"}); r.Status != 422 {
		t.Fatalf("weak password got %d", r.Status)
	}
	if r := h.do("POST", "/api/v1/auth/register", "", map[string]any{"email": "not-an-email", "password": "correct-horse-battery"}); r.Status != 422 {
		t.Fatalf("bad email got %d", r.Status)
	}
	if r := h.do("POST", "/api/v1/auth/login", "", map[string]any{"email": "alice@example.com", "password": "wrong-password-x"}); r.Status != 401 || r.errCode() != "invalid_credentials" {
		t.Fatalf("bad login got %d %s", r.Status, r.Body)
	}
	if r := h.do("POST", "/api/v1/auth/login", "", map[string]any{"email": "nobody@example.com", "password": "wrong-password-x"}); r.Status != 401 || r.errCode() != "invalid_credentials" {
		t.Fatalf("unknown user login got %d", r.Status)
	}
	r := h.do("POST", "/api/v1/auth/login", "", map[string]any{"email": "ALICE@example.com", "password": "correct-horse-battery"})
	if r.Status != 200 {
		t.Fatalf("login got %d %s", r.Status, r.Body)
	}
	var s sessionResp
	r.JSON(t, &s)

	if r := h.do("GET", "/api/v1/auth/me", s.Token, nil); r.Status != 200 {
		t.Fatalf("me got %d", r.Status)
	}
	if r := h.do("GET", "/api/v1/auth/me", "", nil); r.Status != 401 {
		t.Fatalf("anonymous me got %d", r.Status)
	}
	if r := h.do("GET", "/api/v1/auth/me", "garbage", nil); r.Status != 401 {
		t.Fatalf("garbage token got %d", r.Status)
	}
	if r := h.do("POST", "/api/v1/auth/logout", s.Token, nil); r.Status != 204 {
		t.Fatalf("logout got %d", r.Status)
	}
	if r := h.do("GET", "/api/v1/auth/me", s.Token, nil); r.Status != 401 {
		t.Fatalf("revoked token still valid: %d", r.Status)
	}
	// the other session (from registration) is unaffected
	if r := h.do("GET", "/api/v1/auth/me", a.Token, nil); r.Status != 200 {
		t.Fatalf("other session broken: %d", r.Status)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	h := newHarness(t)
	a := h.register("r@example.com")
	r := h.do("POST", "/api/v1/auth/refresh", a.Token, nil)
	if r.Status != 200 {
		t.Fatalf("refresh got %d %s", r.Status, r.Body)
	}
	var s sessionResp
	r.JSON(t, &s)
	if s.Token == "" || s.Token == a.Token {
		t.Fatal("expected a new token")
	}
	if r := h.do("GET", "/api/v1/auth/me", a.Token, nil); r.Status != 401 {
		t.Fatal("old token must be revoked after refresh")
	}
	if r := h.do("GET", "/api/v1/auth/me", s.Token, nil); r.Status != 200 {
		t.Fatal("new token must work")
	}
}

func TestPasswordNotStoredInPlaintext(t *testing.T) {
	h := newHarness(t)
	h.register("p@example.com")
	var hash string
	if err := h.DB.Pool.QueryRow(t.Context(), `SELECT password_hash FROM users WHERE email='p@example.com'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, "correct-horse") {
		t.Fatalf("unexpected hash %q", hash)
	}
	var n int
	if err := h.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE token_hash IS NULL OR length(token_hash) <> 32`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session tokens must be stored as sha256 digests (bad=%d err=%v)", n, err)
	}
}

func TestCSRFRequiredForCookieAuth(t *testing.T) {
	h := newHarness(t)
	a := h.register("c@example.com")
	req := func(withCSRF bool) int {
		rq, _ := http.NewRequest("POST", h.TS.URL+a.wf(""), strings.NewReader(`{"name":"x"}`))
		rq.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.Token})
		rq.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok123"})
		if withCSRF {
			rq.Header.Set(csrfHeader, "tok123")
		}
		res, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if s := req(false); s != 403 {
		t.Fatalf("cookie POST without CSRF got %d", s)
	}
	if s := req(true); s != 201 {
		t.Fatalf("cookie POST with CSRF got %d", s)
	}
	// safe methods do not need it
	rq, _ := http.NewRequest("GET", h.TS.URL+a.wf(""), nil)
	rq.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.Token})
	res, _ := http.DefaultClient.Do(rq)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("cookie GET got %d", res.StatusCode)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	h := newHarness(t)
	r := h.do("POST", "/api/v1/auth/register", "", map[string]any{"email": "k@example.com", "password": "correct-horse-battery"})
	var sc, cc string
	for _, c := range r.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, sessionCookie+"=") {
			sc = c
		}
		if strings.HasPrefix(c, csrfCookie+"=") {
			cc = c
		}
	}
	if !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Lax") {
		t.Fatalf("session cookie flags: %s", sc)
	}
	if strings.Contains(cc, "HttpOnly") {
		t.Fatalf("csrf cookie must be readable by JS: %s", cc)
	}
}

func TestAuthRateLimit(t *testing.T) {
	h := newHarness(t)
	h.Srv.authLimiter.Burst = 3
	h.Srv.authLimiter.Rate = 0.0001
	var last int
	for i := 0; i < 5; i++ {
		last = h.do("POST", "/api/v1/auth/login", "", map[string]any{"email": "x@example.com", "password": "whatever-long-1"}).Status
	}
	if last != 429 {
		t.Fatalf("expected 429 after burst, got %d", last)
	}
}

func TestMalformedBodies(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"unknown field": `{"email":"a@b.co","password":"correct-horse-battery","admin":true}`,
		"not json":      `nope`,
		"trailing":      `{"email":"a@b.co","password":"correct-horse-battery"} {}`,
		"empty":         ``,
	} {
		rq, _ := http.NewRequest("POST", h.TS.URL+"/api/v1/auth/register", strings.NewReader(body))
		res, _ := http.DefaultClient.Do(rq)
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("%s: got %d want 400", name, res.StatusCode)
		}
	}
}
