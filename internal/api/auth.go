package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/persistence"
)

const (
	sessionCookie = "synapse_session"
	csrfCookie    = "synapse_csrf"
	csrfHeader    = "X-CSRF-Token"
)

type authInfo struct {
	User      auth.User
	Session   auth.Session
	Token     string
	ViaCookie bool
}

func userFrom(ctx context.Context) (authInfo, bool) {
	a, ok := ctx.Value(keyUser).(authInfo)
	return a, ok
}

func workspaceFrom(ctx context.Context) (string, auth.Role) {
	w, _ := ctx.Value(keyWorkspace).(wsCtx)
	return w.ID, w.Role
}

type wsCtx struct {
	ID   string
	Role auth.Role
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// requireAuth resolves the caller from a bearer token or session cookie.
// Cookie-authenticated unsafe requests must present the CSRF double-submit token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		viaCookie := false
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(h[7:])
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			token, viaCookie = c.Value, true
		}
		if token == "" {
			writeError(w, r, s.Log, ErrUnauthorized())
			return
		}
		if viaCookie && !isSafeMethod(r.Method) {
			c, err := r.Cookie(csrfCookie)
			h := r.Header.Get(csrfHeader)
			if err != nil || h == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(h)) != 1 {
				writeError(w, r, s.Log, ErrForbidden("missing or invalid CSRF token"))
				return
			}
		}
		u, sess, err := s.Auth.Lookup(r.Context(), token)
		if err != nil {
			if errors.Is(err, persistence.ErrNotFound) {
				writeError(w, r, s.Log, ErrUnauthorized())
				return
			}
			writeError(w, r, s.Log, err)
			return
		}
		ctx := context.WithValue(r.Context(), keyUser, authInfo{User: u, Session: sess, Token: token, ViaCookie: viaCookie})
		next(w, r.WithContext(ctx))
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// requireWorkspace authorizes the caller for the {ws} path segment. Non-members
// get 404 so workspace ids are not enumerable.
func (s *Server) requireWorkspace(min auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("ws")
		a, _ := userFrom(r.Context())
		if !uuidRe.MatchString(id) {
			writeError(w, r, s.Log, ErrNotFound("workspace"))
			return
		}
		role, err := s.Auth.RoleIn(r.Context(), a.User.ID, id)
		if errors.Is(err, persistence.ErrNotFound) {
			writeError(w, r, s.Log, ErrNotFound("workspace"))
			return
		}
		if err != nil {
			writeError(w, r, s.Log, err)
			return
		}
		if !role.AtLeast(min) {
			writeError(w, r, s.Log, ErrForbidden("requires "+string(min)+" role"))
			return
		}
		ctx := context.WithValue(r.Context(), keyWorkspace, wsCtx{ID: id, Role: role})
		next(w, r.WithContext(ctx))
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) setSessionCookies(w http.ResponseWriter, token string, exp time.Time) {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	csrf := base64.RawURLEncoding.EncodeToString(raw[:])
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: exp, HttpOnly: true,
		Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", Expires: exp, HttpOnly: false,
		Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	w.Header().Set(csrfHeader, csrf)
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, n := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1, HttpOnly: n == sessionCookie,
			Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	}
}

func (s *Server) authLimit(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := s.authLimiter.Allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeError(w, r, s.Log, ErrTooMany())
		return false
	}
	return true
}

type registerReq struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	DisplayName   string `json:"display_name"`
	WorkspaceName string `json:"workspace_name"`
}

type sessionResp struct {
	User       auth.User        `json:"user"`
	Workspaces []auth.Workspace `json:"workspaces"`
	Token      string           `json:"token,omitempty"`
	ExpiresAt  time.Time        `json:"expires_at"`
}

func validEmail(e string) bool {
	e = auth.NormalizeEmail(e)
	at := strings.LastIndex(e, "@")
	return len(e) <= 254 && at > 0 && at < len(e)-3 && strings.Contains(e[at:], ".") && !strings.ContainsAny(e, " \t\r\n<>")
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authLimit(w, r) {
		return
	}
	var req registerReq
	if err := decodeJSON(r, &req, 16<<10); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	if !validEmail(req.Email) {
		writeError(w, r, s.Log, ErrUnprocessable("a valid email is required"))
		return
	}
	if len(req.DisplayName) > 100 || len(req.WorkspaceName) > 100 {
		writeError(w, r, s.Log, ErrUnprocessable("name is too long"))
		return
	}
	u, _, err := s.Auth.Register(r.Context(), req.Email, req.Password, req.DisplayName, req.WorkspaceName)
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		writeError(w, r, s.Log, ErrConflict("email already registered"))
		return
	case err != nil && strings.HasPrefix(err.Error(), "password must"):
		writeError(w, r, s.Log, ErrUnprocessable(err.Error()))
		return
	case err != nil:
		writeError(w, r, s.Log, err)
		return
	}
	s.startSession(w, r, u, http.StatusCreated)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u auth.User, status int) {
	token, sess, err := s.Auth.CreateSession(r.Context(), u.ID, r.UserAgent())
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	wss, err := s.Auth.Workspaces(r.Context(), u.ID)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	s.setSessionCookies(w, token, sess.ExpiresAt)
	writeJSON(w, status, sessionResp{User: u, Workspaces: wss, Token: token, ExpiresAt: sess.ExpiresAt})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authLimit(w, r) {
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req, 16<<10); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), req.Email, req.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, r, s.Log, Err(http.StatusUnauthorized, "invalid_credentials", "invalid email or password"))
		return
	}
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	s.startSession(w, r, u, http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	a, _ := userFrom(r.Context())
	if err := s.Auth.Revoke(r.Context(), a.Token); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	a, _ := userFrom(r.Context())
	token, sess, err := s.Auth.Rotate(r.Context(), a.Token, r.UserAgent())
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	wss, _ := s.Auth.Workspaces(r.Context(), a.User.ID)
	s.setSessionCookies(w, token, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, sessionResp{User: a.User, Workspaces: wss, Token: token, ExpiresAt: sess.ExpiresAt})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	a, _ := userFrom(r.Context())
	wss, err := s.Auth.Workspaces(r.Context(), a.User.ID)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResp{User: a.User, Workspaces: wss, ExpiresAt: a.Session.ExpiresAt})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	a, _ := userFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req, 4<<10); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		writeError(w, r, s.Log, ErrUnprocessable("name must be 1-100 characters"))
		return
	}
	ws, err := s.Auth.CreateWorkspace(r.Context(), a.User.ID, req.Name)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	wsID, _ := workspaceFrom(r.Context())
	var req struct {
		Email string    `json:"email"`
		Role  auth.Role `json:"role"`
	}
	if err := decodeJSON(r, &req, 4<<10); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	if req.Role == auth.RoleOwner {
		writeError(w, r, s.Log, ErrUnprocessable("owner role cannot be granted"))
		return
	}
	err := s.Auth.AddMember(r.Context(), wsID, req.Email, req.Role)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		writeError(w, r, s.Log, ErrNotFound("user"))
	case err != nil && err.Error() == "invalid role":
		writeError(w, r, s.Log, ErrUnprocessable("invalid role"))
	case err != nil:
		writeError(w, r, s.Log, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
