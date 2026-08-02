// Package logging builds the structured (JSON) logger used by every binary.
//
// Correlation identifiers (request_id, workflow_id, execution_id, node_id,
// worker_id, attempt) travel on the context so that any log line emitted while
// handling a request or a task can be joined across processes.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

type ctxKey struct{}

// Redactor scrubs registered secret values from arbitrary strings.
type Redactor interface {
	Redact(string) string
}

// New returns a JSON logger tagged with the service name.
func New(service, level string, w io.Writer, r Redactor) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if r != nil && a.Value.Kind() == slog.KindString {
				a.Value = slog.StringValue(r.Redact(a.Value.String()))
			}
			return a
		},
	})
	return slog.New(ctxHandler{h}).With("service", service)
}

// With returns a context carrying additional log attributes.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]any)
	next := make([]any, 0, len(prev)+len(args))
	next = append(next, prev...)
	next = append(next, args...)
	return context.WithValue(ctx, ctxKey{}, next)
}

// ctxHandler injects context attributes into every record.
type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if args, ok := ctx.Value(ctxKey{}).([]any); ok {
		r.Add(args...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(a []slog.Attr) slog.Handler { return ctxHandler{h.Handler.WithAttrs(a)} }
func (h ctxHandler) WithGroup(n string) slog.Handler      { return ctxHandler{h.Handler.WithGroup(n)} }
