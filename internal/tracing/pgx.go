package tracing

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PGX traces queries that run inside an active trace; it never starts roots.
type PGX struct{}

type spanKey struct{}

func (PGX) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	sql := d.SQL
	if len(sql) > 120 {
		sql = sql[:120]
	}
	ctx, span := tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system", "postgresql"), attribute.String("db.statement", sql)))
	return context.WithValue(ctx, spanKey{}, span)
}

func (PGX) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if span, ok := ctx.Value(spanKey{}).(trace.Span); ok {
		End(span, d.Err)
	}
}
