// Package tracing wraps OpenTelemetry: optional OTLP export, HTTP and pgx
// instrumentation, and traceparent persistence for cross-process linkage.
package tracing

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/namesarnav/synapse"

var prop = propagation.TraceContext{}

func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// tracesURL appends the OTLP traces path when the endpoint is a bare base URL.
func tracesURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return endpoint
	}
	u.Path = "/v1/traces"
	return u.String()
}

// Init installs a tracer provider exporting over OTLP/HTTP when endpoint is
// set; otherwise tracing stays a no-op. The returned func flushes and stops it.
func Init(ctx context.Context, service, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	opts := []otlptracehttp.Option{}
	if strings.Contains(endpoint, "://") {
		opts = append(opts, otlptracehttp.WithEndpointURL(tracesURL(endpoint)))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Enabled reports whether a real tracer provider is installed.
func Enabled() bool {
	_, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	return ok
}

// Start begins a span named name.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// End finishes span, recording err when set.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// Traceparent returns the W3C traceparent of the span in ctx, or "".
func Traceparent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	c := propagation.MapCarrier{}
	prop.Inject(ctx, c)
	return c.Get("traceparent")
}

// WithTraceparent returns ctx carrying the remote span context tp.
func WithTraceparent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	return prop.Extract(ctx, propagation.MapCarrier{"traceparent": tp})
}

// HTTP traces each request as a server span and honours an incoming traceparent.
func HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer().Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		rec := &statusWriter{ResponseWriter: w}
		r2 := r.WithContext(ctx)
		next.ServeHTTP(rec, r2)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		if r2.Pattern != "" {
			SetRoute(ctx, r2.Pattern)
		}
		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		span.End()
	})
}

// SetRoute names the active server span after the matched route pattern.
func SetRoute(ctx context.Context, route string) {
	span := trace.SpanFromContext(ctx)
	span.SetName(route)
	span.SetAttributes(attribute.String("http.route", route))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
