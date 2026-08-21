package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/namesarnav/synapse/internal/logging"
	"github.com/namesarnav/synapse/internal/tracing"
)

// APIError is the structured error returned to clients:
//
//	{"error": {"code": "not_found", "message": "...", "details": {...}, "request_id": "..."}}
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func Err(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

func (e *APIError) With(details any) *APIError {
	c := *e
	c.Details = details
	return &c
}

var (
	ErrBadRequest    = func(msg string) *APIError { return Err(http.StatusBadRequest, "bad_request", msg) }
	ErrUnauthorized  = func() *APIError { return Err(http.StatusUnauthorized, "unauthorized", "authentication required") }
	ErrForbidden     = func(msg string) *APIError { return Err(http.StatusForbidden, "forbidden", msg) }
	ErrNotFound      = func(what string) *APIError { return Err(http.StatusNotFound, "not_found", what+" not found") }
	ErrConflict      = func(msg string) *APIError { return Err(http.StatusConflict, "conflict", msg) }
	ErrUnprocessable = func(msg string) *APIError {
		return Err(http.StatusUnprocessableEntity, "unprocessable", msg)
	}
	ErrTooMany = func() *APIError { return Err(http.StatusTooManyRequests, "rate_limited", "too many requests") }
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyUser
	keyWorkspace
)

// RequestID returns the request id stored on ctx.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		log.ErrorContext(r.Context(), "unhandled error", "error", err, "path", r.URL.Path)
		ae = Err(http.StatusInternalServerError, "internal", "internal server error")
	}
	body := map[string]any{"error": map[string]any{
		"code": ae.Code, "message": ae.Message, "details": ae.Details, "request_id": RequestID(r.Context()),
	}}
	writeJSON(w, ae.Status, body)
}

// decodeJSON reads a size-limited JSON body, rejecting unknown fields.
func decodeJSON(r *http.Request, dst any, maxBytes int64) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrBadRequest("request body is required")
		}
		return ErrBadRequest("invalid JSON: " + err.Error())
	}
	if dec.More() {
		return ErrBadRequest("unexpected trailing data")
	}
	return nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(c int) {
	if s.status == 0 {
		s.status = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = 200
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Hijack/Flush pass-throughs keep WebSockets and streaming working.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withRequestID attaches (or accepts) a request id and correlates logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = newID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), keyRequestID, id)
		ctx = logging.With(ctx, "request_id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func withAccessLog(log *slog.Logger, observe func(method, route string, status int, d time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = 200
			}
			d := time.Since(start)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			tracing.SetRoute(r.Context(), route)
			if observe != nil {
				observe(r.Method, route, rec.status, d)
			}
			if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" || r.URL.Path == "/metrics" {
				return
			}
			log.InfoContext(r.Context(), "http request", "method", r.Method, "path", r.URL.Path,
				"route", route, "status", rec.status, "bytes", rec.bytes, "duration_ms", d.Milliseconds())
		})
	}
}

func withRecover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.ErrorContext(r.Context(), "panic in handler", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
					writeError(w, r, log, Err(http.StatusInternalServerError, "internal", "internal server error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
