package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

// Context is the data expressions can see during an execution.
type Context struct {
	ExecutionID string
	WorkflowID  string
	Version     int
	Trigger     any
	Nodes       map[string]any
	Item        any
	Index       int
	InLoop      bool
}

// Env builds the expression environment. secret may be nil.
func (c *Context) Env(secret func(string) (string, bool), used map[string]bool, now func() time.Time) *expressions.Env {
	vars := map[string]any{
		"trigger": c.Trigger,
		"nodes":   c.nodesOrEmpty(),
		"execution": map[string]any{
			"id": c.ExecutionID, "workflow_id": c.WorkflowID, "version": float64(c.Version),
		},
	}
	if c.InLoop {
		vars["item"] = c.Item
		vars["index"] = float64(c.Index)
	}
	return &expressions.Env{Vars: vars, Secret: secret, SecretsUsed: used, Now: now}
}

func (c *Context) nodesOrEmpty() map[string]any {
	if c.Nodes == nil {
		return map[string]any{}
	}
	return c.Nodes
}

// NodeError describes why a node failed.
type NodeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *NodeError) Error() string { return e.Code + ": " + e.Message }

// Error codes shared by executors and the runtime.
const (
	CodeConfig     = "config_error"
	CodeExpression = "expression_error"
	CodeTimeout    = "timeout"
	CodeHTTP       = "http_error"
	CodeNetwork    = "network_error"
	CodeNodeFailed = "node_failed"
	CodeStopped    = "stopped"
	CodeChild      = "child_failed"
	CodeLeaseLost  = "delivery_exhausted"
	CodeInternal   = "internal_error"
)

// NodeRefs returns the node ids referenced by a node's expressions/templates.
func NodeRefs(n *workflow.Node) []string {
	exprs, tmpls := workflow.Sources(n)
	seen := map[string]bool{}
	for _, s := range exprs {
		if e, err := expressions.Parse(s); err == nil {
			for _, r := range e.References().Nodes {
				seen[r] = true
			}
		}
	}
	for _, s := range tmpls {
		if !strings.Contains(s, "{{") {
			continue
		}
		if t, err := expressions.ParseTemplate(s); err == nil {
			for _, r := range t.References().Nodes {
				seen[r] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func exprErr(err error) *NodeError {
	return &NodeError{Code: CodeExpression, Message: err.Error()}
}

func cfgErr(err error) *NodeError {
	return &NodeError{Code: CodeConfig, Message: fmt.Sprint(err)}
}
