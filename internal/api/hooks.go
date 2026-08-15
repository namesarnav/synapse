package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/secrets"
	"github.com/namesarnav/synapse/internal/triggers"
)

const signatureHeader = "X-Synapse-Signature"

// Only these request headers are stored on the execution; auth-bearing ones never are.
var hookHeaderAllowlist = []string{"Content-Type", "User-Agent", "X-Request-Id", "Idempotency-Key",
	"X-Synapse-Delivery", "X-Github-Event", "X-Github-Delivery", "X-Event-Type"}

// handleWebhook is the public ingress for webhook triggers.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("endpoint_id")
	if ok, retry := s.hookLimiter.Allow(id); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, r, s.Log, ErrTooMany())
		return
	}
	ep, err := s.Triggers.Endpoint(r.Context(), id)
	if err != nil {
		if errors.Is(err, triggers.ErrNoEndpoint) {
			writeError(w, r, s.Log, ErrNotFound("webhook"))
			return
		}
		writeError(w, r, s.Log, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Cfg.WebhookMaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, s.Log, Err(http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large"))
			return
		}
		writeError(w, r, s.Log, ErrBadRequest("could not read request body"))
		return
	}
	if ep.HMACSecret != "" {
		secret, serr := s.Secrets.Get(r.Context(), ep.WorkspaceID, ep.HMACSecret)
		if serr != nil {
			// Fail closed and do not reveal whether the secret is missing.
			s.Log.Warn("webhook secret unavailable", "endpoint", ep.ID, "err", serr)
			writeError(w, r, s.Log, Err(http.StatusUnauthorized, "invalid_signature", "signature verification failed"))
			return
		}
		if !triggers.VerifySignature(secret, body, r.Header.Get(signatureHeader)) {
			writeError(w, r, s.Log, Err(http.StatusUnauthorized, "invalid_signature", "signature verification failed"))
			return
		}
	}
	payload := map[string]any{
		"method": r.Method, "headers": allowedHeaders(r.Header), "query": queryMap(r),
		"body": decodeBody(r.Header.Get("Content-Type"), body), "received_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "json") && len(body) > 0 && !json.Valid(body) {
		writeError(w, r, s.Log, ErrBadRequest("body is not valid JSON"))
		return
	}
	key := firstHeader(r.Header, "Idempotency-Key", "X-Synapse-Delivery", "X-Github-Delivery")
	if len(key) > 200 {
		writeError(w, r, s.Log, ErrBadRequest("idempotency key is too long"))
		return
	}
	p := runtime.StartParams{WorkspaceID: ep.WorkspaceID, WorkflowID: ep.WorkflowID, TriggerType: "webhook", Trigger: payload, StartNode: ep.NodeID}
	if key != "" {
		p.IdempotencyKey = "webhook:" + ep.ID + ":" + key
	}
	res, err := s.Runtime.Start(r.Context(), p)
	switch {
	case errors.Is(err, runtime.ErrNotPublished):
		writeError(w, r, s.Log, ErrNotFound("webhook"))
		return
	case err != nil:
		s.runtimeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"execution_id": res.Execution.ID, "duplicate": res.Duplicate})
}

func allowedHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range hookHeaderAllowlist {
		if v := h.Get(k); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

func queryMap(r *http.Request) map[string]any {
	out := map[string]any{}
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out
}

func decodeBody(contentType string, body []byte) any {
	if len(body) == 0 {
		return nil
	}
	if strings.Contains(contentType, "json") {
		var v any
		if json.Unmarshal(body, &v) == nil {
			return v
		}
	}
	return string(body)
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// handleListWebhooks lists a workflow's endpoints with their public URL paths.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	id, ok := s.wfID(w, r)
	if !ok {
		return
	}
	eps, err := s.Triggers.Endpoints(r.Context(), ws, id)
	if err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	type item struct {
		ID     string `json:"id"`
		NodeID string `json:"node_id"`
		Path   string `json:"path"`
		Signed bool   `json:"signed"`
	}
	out := make([]item, 0, len(eps))
	for _, e := range eps {
		out = append(out, item{ID: e.ID, NodeID: e.NodeID, Path: "/hooks/" + e.ID, Signed: e.HMACSecret != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

// --- secrets ---

func (s *Server) secretErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		writeError(w, r, s.Log, ErrNotFound("secret"))
	case errors.Is(err, secrets.ErrInvalidName), errors.Is(err, secrets.ErrEmptyValue), errors.Is(err, secrets.ErrTooLarge):
		writeError(w, r, s.Log, ErrUnprocessable(err.Error()))
	default:
		writeError(w, r, s.Log, err)
	}
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	list, err := s.Secrets.List(r.Context(), ws)
	if err != nil {
		s.secretErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": list})
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	a, _ := userFrom(r.Context())
	var b struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &b, 32<<10); err != nil {
		writeError(w, r, s.Log, err)
		return
	}
	if err := s.Secrets.Set(r.Context(), ws, r.PathValue("name"), b.Value, a.User.ID); err != nil {
		s.secretErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	ws, _ := workspaceFrom(r.Context())
	if err := s.Secrets.Delete(r.Context(), ws, r.PathValue("name")); err != nil {
		s.secretErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
