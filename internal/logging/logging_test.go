package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

type redactor struct{}

func (redactor) Redact(s string) string { return strings.ReplaceAll(s, "hunter2", "***") }

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

func TestContextAttributesAndRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := New("svc", "info", &buf, redactor{})
	ctx := With(With(context.Background(), "request_id", "r1"), "execution_id", "e1")
	log.InfoContext(ctx, "hello", "token", "pw is hunter2")
	m := decode(t, buf.Bytes())
	if m["service"] != "svc" || m["request_id"] != "r1" || m["execution_id"] != "e1" {
		t.Fatalf("missing attributes: %v", m)
	}
	if m["token"] != "pw is ***" {
		t.Fatalf("secret leaked: %v", m["token"])
	}
}

func TestTraceIDsAreAdded(t *testing.T) {
	var buf bytes.Buffer
	log := New("svc", "info", &buf, nil)
	tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	log.InfoContext(ctx, "x")
	m := decode(t, buf.Bytes())
	if m["trace_id"] != tid.String() || m["span_id"] != sid.String() {
		t.Fatalf("trace ids missing: %v", m)
	}
	buf.Reset()
	log.InfoContext(context.Background(), "y")
	if _, ok := decode(t, buf.Bytes())["trace_id"]; ok {
		t.Fatal("trace_id present without a span")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := New("svc", "warn", &buf, nil)
	log.Info("quiet")
	if buf.Len() != 0 {
		t.Fatalf("info logged at warn level: %s", buf.String())
	}
	log.Warn("loud")
	if buf.Len() == 0 {
		t.Fatal("warn not logged")
	}
}
