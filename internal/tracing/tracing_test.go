package tracing

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func useRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return sr
}

func TestTraceparentRoundTrip(t *testing.T) {
	useRecorder(t)
	ctx, span := Start(t.Context(), "a")
	defer span.End()
	tp := Traceparent(ctx)
	if tp == "" {
		t.Fatal("no traceparent from an active span")
	}
	remote := WithTraceparent(t.Context(), tp)
	_, child := Start(remote, "b")
	defer child.End()
	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Fatal("child did not join the trace")
	}
}

func TestNoTraceparentWithoutSpan(t *testing.T) {
	if tp := Traceparent(t.Context()); tp != "" {
		t.Fatalf("unexpected traceparent %q", tp)
	}
	if ctx := WithTraceparent(t.Context(), ""); ctx == nil {
		t.Fatal("nil ctx")
	}
}

func TestHTTPMiddlewareHonoursIncomingTraceAndNamesRoute(t *testing.T) {
	sr := useRecorder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, s := Start(r.Context(), "inner", attribute.String("k", "v"))
		s.End()
		w.WriteHeader(http.StatusTeapot)
	})
	req := httptest.NewRequest("GET", "/items/7", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	HTTP(mux).ServeHTTP(httptest.NewRecorder(), req)
	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	srv := spans[1]
	if srv.Name() != "GET /items/{id}" {
		t.Fatalf("name = %q", srv.Name())
	}
	if srv.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatal("incoming trace id not honoured")
	}
	if spans[0].Parent().SpanID() != srv.SpanContext().SpanID() {
		t.Fatal("inner span not parented to the server span")
	}
}

func TestInitDisabledWithoutEndpoint(t *testing.T) {
	down, err := Init(t.Context(), "svc", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := down(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTracesURL(t *testing.T) {
	cases := map[string]string{
		"http://jaeger:4318":              "http://jaeger:4318/v1/traces",
		"http://jaeger:4318/":             "http://jaeger:4318/v1/traces",
		"https://otel.example.com/custom": "https://otel.example.com/custom",
		"http://jaeger:4318/v1/traces":    "http://jaeger:4318/v1/traces",
	}
	for in, want := range cases {
		if got := tracesURL(in); got != want {
			t.Errorf("tracesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
