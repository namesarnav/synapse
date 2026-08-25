package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/namesarnav/synapse/internal/ratelimit"
	"github.com/namesarnav/synapse/internal/triggers"
)

var hookGraph = map[string]any{
	"nodes": []map[string]any{
		{"id": "h", "type": "webhook_trigger", "config": map[string]any{"hmac_secret": "HOOK_KEY"}},
		{"id": "log", "type": "log", "config": map[string]any{"message": "got hook"}},
	},
	"edges": []map[string]any{{"source": "h", "target": "log"}},
}

func (h *harness) postHook(id string, body []byte, headers map[string]string) resp {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.TS.URL+"/hooks/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return resp{Status: res.StatusCode, Body: buf.Bytes(), Header: res.Header}
}

func (h *harness) publishHook(a account, graph map[string]any) (wfID, endpoint string) {
	h.t.Helper()
	var wf struct{ ID string }
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "hooked", "graph": graph}).JSON(h.t, &wf)
	if r := h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, nil); r.Status != 201 {
		h.t.Fatalf("publish: %d %s", r.Status, r.Body)
	}
	var list struct {
		Webhooks []struct {
			ID     string
			Path   string
			Signed bool
		}
	}
	h.do("GET", a.wf("/"+wf.ID+"/webhooks"), a.Token, nil).JSON(h.t, &list)
	if len(list.Webhooks) != 1 {
		h.t.Fatalf("webhooks = %+v", list)
	}
	return wf.ID, list.Webhooks[0].ID
}

func (h *harness) putSecret(a account, name, value string) resp {
	return h.do("PUT", fmt.Sprintf("/api/v1/workspaces/%s/secrets/%s", a.WorkspaceID, name), a.Token, map[string]any{"value": value})
}

func TestWebhookSignatureAndDedupe(t *testing.T) {
	h := newHarness(t)
	a := h.register("hook@example.com")
	if r := h.putSecret(a, "HOOK_KEY", "whsec-123456"); r.Status != 204 {
		t.Fatalf("put secret: %d %s", r.Status, r.Body)
	}
	wfID, ep := h.publishHook(a, hookGraph)
	body := []byte(`{"event":"ping","n":1}`)

	if r := h.postHook(ep, body, nil); r.Status != 401 || r.errCode() != "invalid_signature" {
		t.Fatalf("unsigned: %d %s", r.Status, r.Body)
	}
	if r := h.postHook(ep, body, map[string]string{signatureHeader: triggers.Sign("wrong", body)}); r.Status != 401 {
		t.Fatalf("bad signature: %d", r.Status)
	}
	good := map[string]string{signatureHeader: triggers.Sign("whsec-123456", body), "Idempotency-Key": "d-1",
		"Authorization": "Bearer leak-me", "Cookie": "sid=leak-me"}
	r1 := h.postHook(ep, body, good)
	r2 := h.postHook(ep, body, good)
	if r1.Status != 202 || r2.Status != 202 {
		t.Fatalf("good: %d %s / %d %s", r1.Status, r1.Body, r2.Status, r2.Body)
	}
	var b1, b2 struct {
		ExecutionID string `json:"execution_id"`
		Duplicate   bool
	}
	r1.JSON(t, &b1)
	r2.JSON(t, &b2)
	if b1.ExecutionID != b2.ExecutionID || b1.Duplicate || !b2.Duplicate {
		t.Fatalf("dedupe: %+v %+v", b1, b2)
	}

	var got struct {
		Execution struct {
			TriggerType    string         `json:"trigger_type"`
			TriggerPayload map[string]any `json:"trigger_payload"`
		}
	}
	h.do("GET", a.ex("/"+b1.ExecutionID), a.Token, nil).JSON(t, &got)
	if got.Execution.TriggerType != "webhook" {
		t.Fatalf("trigger = %+v", got.Execution)
	}
	raw := fmt.Sprint(got.Execution.TriggerPayload)
	if strings.Contains(raw, "leak-me") || strings.Contains(raw, "sha256=") {
		t.Fatalf("sensitive headers persisted: %s", raw)
	}
	if got.Execution.TriggerPayload["body"].(map[string]any)["event"] != "ping" {
		t.Fatalf("body = %v", got.Execution.TriggerPayload["body"])
	}
	// A different delivery id is a new execution.
	good["Idempotency-Key"] = "d-2"
	var b3 struct {
		ExecutionID string `json:"execution_id"`
	}
	h.postHook(ep, body, good).JSON(t, &b3)
	if b3.ExecutionID == b1.ExecutionID {
		t.Fatal("distinct deliveries collapsed")
	}
	_ = wfID
}

func TestWebhookMissingSecretFailsClosed(t *testing.T) {
	h := newHarness(t)
	a := h.register("nosecret@example.com")
	_, ep := h.publishHook(a, hookGraph) // HOOK_KEY was never created
	body := []byte(`{}`)
	if r := h.postHook(ep, body, map[string]string{signatureHeader: triggers.Sign("", body)}); r.Status != 401 {
		t.Fatalf("status = %d", r.Status)
	}
}

func TestWebhookUnsignedEndpoint(t *testing.T) {
	h := newHarness(t)
	a := h.register("open@example.com")
	g := map[string]any{
		"nodes": []map[string]any{{"id": "h", "type": "webhook_trigger"}, {"id": "log", "type": "log", "config": map[string]any{"message": "x"}}},
		"edges": []map[string]any{{"source": "h", "target": "log"}},
	}
	wfID, ep := h.publishHook(a, g)
	if r := h.postHook(ep, []byte(`{"a":1}`), nil); r.Status != 202 {
		t.Fatalf("status = %d %s", r.Status, r.Body)
	}
	if r := h.postHook(ep, []byte(`{not json`), nil); r.Status != 400 {
		t.Fatalf("invalid json = %d", r.Status)
	}
	if r := h.postHook(ep, bytes.Repeat([]byte("a"), 2048), map[string]string{"Content-Type": "text/plain"}); r.Status != 413 {
		t.Fatalf("oversize = %d", r.Status)
	}
	if r := h.postHook("00000000-0000-0000-0000-000000000000", nil, nil); r.Status != 404 {
		t.Fatalf("unknown = %d", r.Status)
	}
	if r := h.postHook("nope", nil, nil); r.Status != 404 {
		t.Fatalf("malformed id = %d", r.Status)
	}
	// Unpublished workflows stop accepting hooks.
	h.do("POST", a.wf("/"+wfID+"/unpublish"), a.Token, nil)
	if r := h.postHook(ep, []byte(`{}`), nil); r.Status != 404 {
		t.Fatalf("after unpublish = %d", r.Status)
	}
}

func TestWebhookRateLimit(t *testing.T) {
	h := newHarness(t)
	h.Srv.hookLimiter = ratelimit.New(0.001, 2)
	a := h.register("rate@example.com")
	g := map[string]any{
		"nodes": []map[string]any{{"id": "h", "type": "webhook_trigger"}, {"id": "log", "type": "log", "config": map[string]any{"message": "x"}}},
		"edges": []map[string]any{{"source": "h", "target": "log"}},
	}
	_, ep := h.publishHook(a, g)
	codes := []int{}
	for i := 0; i < 4; i++ {
		codes = append(codes, h.postHook(ep, []byte(`{}`), nil).Status)
	}
	if codes[0] != 202 || codes[1] != 202 || codes[2] != 429 || codes[3] != 429 {
		t.Fatalf("codes = %v", codes)
	}
	if r := h.postHook(ep, []byte(`{}`), nil); r.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestSecretsAPI(t *testing.T) {
	h := newHarness(t)
	a := h.register("sec@example.com")
	other := h.register("sec2@example.com")
	if r := h.putSecret(a, "TOKEN", "value-abcdef"); r.Status != 204 {
		t.Fatalf("put = %d %s", r.Status, r.Body)
	}
	if r := h.putSecret(a, "bad-name", "x"); r.Status != 422 {
		t.Fatalf("bad name = %d", r.Status)
	}
	path := fmt.Sprintf("/api/v1/workspaces/%s/secrets", a.WorkspaceID)
	r := h.do("GET", path, a.Token, nil)
	if r.Status != 200 || strings.Contains(string(r.Body), "value-abcdef") || !strings.Contains(string(r.Body), "TOKEN") {
		t.Fatalf("list = %d %s", r.Status, r.Body)
	}
	if r := h.do("GET", path, other.Token, nil); r.Status != 404 {
		t.Fatalf("cross-workspace list = %d", r.Status)
	}
	if r := h.do("DELETE", path+"/TOKEN", a.Token, nil); r.Status != 204 {
		t.Fatalf("delete = %d", r.Status)
	}
	if r := h.do("DELETE", path+"/TOKEN", a.Token, nil); r.Status != 404 {
		t.Fatalf("second delete = %d", r.Status)
	}
}

func TestScheduleCronValidatedOnPublish(t *testing.T) {
	h := newHarness(t)
	a := h.register("cron@example.com")
	mk := func(cron string) map[string]any {
		return map[string]any{
			"nodes": []map[string]any{{"id": "s", "type": "schedule_trigger", "config": map[string]any{"cron": cron}}, {"id": "log", "type": "log", "config": map[string]any{"message": "x"}}},
			"edges": []map[string]any{{"source": "s", "target": "log"}},
		}
	}
	var bad struct{ ID string }
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "bad", "graph": mk("61 * * * *")}).JSON(t, &bad)
	if r := h.do("POST", a.wf("/"+bad.ID+"/publish"), a.Token, nil); r.Status != 422 {
		t.Fatalf("bad cron publish = %d %s", r.Status, r.Body)
	}
	var good struct{ ID string }
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "good", "graph": mk("*/10 * * * *")}).JSON(t, &good)
	if r := h.do("POST", a.wf("/"+good.ID+"/publish"), a.Token, nil); r.Status != 201 {
		t.Fatalf("good cron publish = %d %s", r.Status, r.Body)
	}
	var n int
	_ = h.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM schedules WHERE workflow_id=$1`, good.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("schedule rows = %d", n)
	}
}

func TestSecretsRoleEnforcement(t *testing.T) {
	h := newHarness(t)
	owner := h.register("sec-owner@example.com")
	viewer := h.register("sec-viewer@example.com")
	member := h.register("sec-member@example.com")
	members := fmt.Sprintf("/api/v1/workspaces/%s/members", owner.WorkspaceID)
	for _, m := range []struct {
		a    account
		role string
	}{{viewer, "viewer"}, {member, "member"}} {
		if r := h.do("POST", members, owner.Token, map[string]any{"email": m.a.Email, "role": m.role}); r.Status != 204 {
			t.Fatalf("add %s: %d %s", m.role, r.Status, r.Body)
		}
	}
	if r := h.putSecret(owner, "TOKEN", "value-abcdef"); r.Status != 204 {
		t.Fatalf("owner put = %d", r.Status)
	}
	path := fmt.Sprintf("/api/v1/workspaces/%s/secrets", owner.WorkspaceID)
	put := func(a account) resp {
		return h.do("PUT", path+"/OTHER", a.Token, map[string]any{"value": "x-value"})
	}
	// Viewers cannot see that secrets exist, let alone change them.
	if r := h.do("GET", path, viewer.Token, nil); r.Status != 403 {
		t.Errorf("viewer list = %d", r.Status)
	}
	if r := put(viewer); r.Status != 403 {
		t.Errorf("viewer put = %d", r.Status)
	}
	if r := h.do("DELETE", path+"/TOKEN", viewer.Token, nil); r.Status != 403 {
		t.Errorf("viewer delete = %d", r.Status)
	}
	// Members can list names but only admins write.
	if r := h.do("GET", path, member.Token, nil); r.Status != 200 || strings.Contains(string(r.Body), "value-abcdef") {
		t.Errorf("member list = %d %s", r.Status, r.Body)
	}
	if r := put(member); r.Status != 403 {
		t.Errorf("member put = %d", r.Status)
	}
	if r := h.do("DELETE", path+"/TOKEN", member.Token, nil); r.Status != 403 {
		t.Errorf("member delete = %d", r.Status)
	}
	if r := h.do("GET", path, owner.Token, nil); !strings.Contains(string(r.Body), "TOKEN") {
		t.Errorf("secret vanished after forbidden deletes: %s", r.Body)
	}
}
