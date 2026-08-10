// Package nodes implements the executors for non-inline node types. Executors
// run on workers, must be safe for concurrent use and honour ctx cancellation.
package nodes

import (
	"context"
	"log/slog"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

// Input is everything an executor needs for one attempt.
type Input struct {
	Node    *workflow.Node
	Config  any
	Env     *expressions.Env
	Attempt int
	// IdempotencyKey is stable across attempts' redeliveries of the same
	// (execution, node, attempt) and changes between attempts.
	IdempotencyKey string
	Log            *slog.Logger
}

// Executor runs one node attempt. A returned *engine.NodeError controls retry
// classification; any other error is treated as a retryable internal error.
type Executor interface {
	Execute(ctx context.Context, in Input) (any, error)
}

type ExecutorFunc func(ctx context.Context, in Input) (any, error)

func (f ExecutorFunc) Execute(ctx context.Context, in Input) (any, error) { return f(ctx, in) }

// Registry maps node types to executors.
type Registry map[workflow.NodeType]Executor

// Options configure the built-in executors.
type Options struct {
	HTTP   HTTPOptions
	Mailer Mailer
	Now    func() time.Time
}

// NewRegistry returns the built-in executors.
func NewRegistry(o Options) Registry {
	if o.Mailer == nil {
		o.Mailer = NewMockMailer()
	}
	return Registry{
		workflow.TypeTransform:   ExecutorFunc(execTransform),
		workflow.TypeLog:         ExecutorFunc(execLog),
		workflow.TypeEmail:       &emailExec{mailer: o.Mailer},
		workflow.TypeHTTPRequest: NewHTTPExecutor(o.HTTP),
	}
}

func exprFailure(err error) error {
	return &engine.NodeError{Code: engine.CodeExpression, Message: err.Error()}
}
