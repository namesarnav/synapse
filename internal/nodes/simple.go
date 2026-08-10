package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

func execTransform(_ context.Context, in Input) (any, error) {
	c := in.Config.(*workflow.TransformConfig)
	if c.Expression != "" {
		v, err := expressions.EvalString(c.Expression, in.Env)
		if err != nil {
			return nil, exprFailure(err)
		}
		return v, nil
	}
	v, err := expressions.Resolve(expressions.Normalize(c.Output), in.Env)
	if err != nil {
		return nil, exprFailure(err)
	}
	return v, nil
}

func execLog(_ context.Context, in Input) (any, error) {
	c := in.Config.(*workflow.LogConfig)
	msg, err := expressions.RenderTemplate(c.Message, in.Env)
	if err != nil {
		return nil, exprFailure(err)
	}
	fields, err := expressions.Resolve(expressions.Normalize(c.Fields), in.Env)
	if err != nil {
		return nil, exprFailure(err)
	}
	text := expressions.ToString(msg)
	args := []any{"node", in.Node.ID, "fields", fields}
	switch c.Level {
	case "debug":
		in.Log.Debug(text, args...)
	case "warn":
		in.Log.Warn(text, args...)
	case "error":
		in.Log.Error(text, args...)
	default:
		in.Log.Info(text, args...)
	}
	return map[string]any{"message": text, "fields": fields}, nil
}

// Mail is one outgoing message.
type Mail struct {
	To, From, Subject, Body string
}

// Mailer delivers email; the built-in implementation is a mock.
type Mailer interface {
	Send(ctx context.Context, m Mail) (id string, err error)
}

// MockMailer records messages instead of sending them.
type MockMailer struct {
	mu   sync.Mutex
	sent []Mail
}

func NewMockMailer() *MockMailer { return &MockMailer{} }

func (m *MockMailer) Send(_ context.Context, mail Mail) (string, error) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	m.mu.Lock()
	m.sent = append(m.sent, mail)
	m.mu.Unlock()
	return "mock-" + hex.EncodeToString(b[:]), nil
}

// Sent returns a copy of the recorded messages.
func (m *MockMailer) Sent() []Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Mail(nil), m.sent...)
}

type emailExec struct{ mailer Mailer }

func (e *emailExec) Execute(ctx context.Context, in Input) (any, error) {
	c := in.Config.(*workflow.EmailConfig)
	render := func(s string) (string, error) {
		v, err := expressions.RenderTemplate(s, in.Env)
		if err != nil {
			return "", exprFailure(err)
		}
		return expressions.ToString(v), nil
	}
	var m Mail
	var err error
	if m.To, err = render(c.To); err != nil {
		return nil, err
	}
	if m.From, err = render(c.From); err != nil {
		return nil, err
	}
	if m.Subject, err = render(c.Subject); err != nil {
		return nil, err
	}
	if m.Body, err = render(c.Body); err != nil {
		return nil, err
	}
	if m.From == "" {
		m.From = "synapse@localhost"
	}
	id, err := e.mailer.Send(ctx, m)
	if err != nil {
		return nil, &engine.NodeError{Code: engine.CodeNetwork, Message: err.Error(), Retryable: true}
	}
	return map[string]any{"provider": "mock", "message_id": id, "to": m.To, "subject": m.Subject, "sent_at": time.Now().UTC().Format(time.RFC3339)}, nil
}
