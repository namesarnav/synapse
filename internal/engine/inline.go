package engine

import (
	"time"

	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

// InlineKind says what the runtime must do with an inline node.
type InlineKind int

const (
	InlineDone    InlineKind = iota // node completed with Output/Branch
	InlineWait                      // durable wait until WakeAt
	InlineFail                      // node failed with Err
	InlineStop                      // terminate the whole execution
	InlineForEach                   // fan out over Items
	InlineSub                       // start a child workflow
)

// InlineResult is the pure outcome of evaluating an inline node.
type InlineResult struct {
	Kind   InlineKind
	Output any
	Branch string
	Err    *NodeError
	WakeAt time.Time
	// Stop
	StopStatus string
	// ForEach
	Items       []any
	Concurrency int
	OnItemError string
	// Sub-workflow
	SubWorkflowID string
	SubInput      any
}

// EvalInline evaluates a node the engine executes itself. sources are the
// outputs of the active incoming edges (used by merge and triggers).
func EvalInline(n *workflow.Node, ctx *Context, env *expressions.Env, now time.Time, sources map[string]any) InlineResult {
	cfg, err := workflow.DecodeConfig(n)
	if err != nil {
		return failRes(cfgErr(err))
	}
	switch c := cfg.(type) {
	case *workflow.WebhookConfig, *workflow.ScheduleConfig, *workflow.ManualConfig:
		return InlineResult{Kind: InlineDone, Output: ctx.Trigger}
	case *struct{}:
		return InlineResult{Kind: InlineDone, Output: ctx.Item}
	case *workflow.ConditionConfig:
		v, err := expressions.EvalString(c.Expression, env)
		if err != nil {
			return failRes(exprErr(err))
		}
		ok := expressions.Truthy(v)
		br := workflow.BranchFalse
		if ok {
			br = workflow.BranchTrue
		}
		return InlineResult{Kind: InlineDone, Output: map[string]any{"result": ok}, Branch: br}
	case *workflow.MergeConfig:
		return InlineResult{Kind: InlineDone, Output: sources}
	case *workflow.StopConfig:
		msg := ""
		if c.Message != "" {
			s, err := expressions.RenderTemplate(c.Message, env)
			if err != nil {
				return failRes(exprErr(err))
			}
			msg = expressions.ToString(s)
		}
		st := c.Status
		if st == "" {
			st = "succeeded"
		}
		return InlineResult{Kind: InlineStop, StopStatus: st, Output: map[string]any{"status": st, "message": msg}}
	case *workflow.DelayConfig:
		wake := now
		if d, ok, err := c.Wait(); err != nil {
			return failRes(cfgErr(err))
		} else if ok {
			wake = now.Add(d)
		} else {
			v, err := expressions.EvalString(c.Until, env)
			if err != nil {
				return failRes(exprErr(err))
			}
			s, _ := v.(string)
			t, perr := time.Parse(time.RFC3339Nano, s)
			if perr != nil {
				return failRes(&NodeError{Code: CodeExpression, Message: "until must evaluate to an RFC 3339 time"})
			}
			wake = t
			if wake.After(now.Add(workflow.MaxDelay)) {
				return failRes(&NodeError{Code: CodeConfig, Message: "delay exceeds the 30 day maximum"})
			}
		}
		out := map[string]any{"wake_at": wake.UTC().Format(time.RFC3339Nano)}
		if !wake.After(now) {
			return InlineResult{Kind: InlineDone, Output: out}
		}
		return InlineResult{Kind: InlineWait, WakeAt: wake, Output: out}
	case *workflow.ForEachConfig:
		v, err := expressions.EvalString(c.Items, env)
		if err != nil {
			return failRes(exprErr(err))
		}
		var items []any
		switch t := v.(type) {
		case nil:
		case []any:
			items = t
		default:
			return failRes(&NodeError{Code: CodeExpression, Message: "items must evaluate to an array, got " + typeOf(v)})
		}
		max := c.MaxItems
		if max == 0 {
			max = workflow.DefaultForEachMaxItems
		}
		if len(items) > max {
			return failRes(&NodeError{Code: CodeConfig, Message: "foreach has more items than max_items"})
		}
		conc := c.Concurrency
		if conc == 0 {
			conc = workflow.DefaultForEachConc
		}
		onErr := c.OnItemError
		if onErr == "" {
			onErr = workflow.OnErrorFail
		}
		return InlineResult{Kind: InlineForEach, Items: items, Concurrency: conc, OnItemError: onErr, Branch: workflow.BranchDone}
	case *workflow.SubWorkflowConfig:
		in, err := expressions.Resolve(c.Input, env)
		if err != nil {
			return failRes(exprErr(err))
		}
		return InlineResult{Kind: InlineSub, SubWorkflowID: c.WorkflowID, SubInput: in}
	}
	return failRes(&NodeError{Code: CodeInternal, Message: "node type is not inline: " + string(n.Type)})
}

func failRes(e *NodeError) InlineResult { return InlineResult{Kind: InlineFail, Err: e} }

func typeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	}
	return "value"
}

// ForEachOutput builds the aggregated output of a foreach node.
func ForEachOutput(results []any, failed int) map[string]any {
	return map[string]any{"items": results, "count": float64(len(results)), "failed": float64(failed)}
}

// ResultOf returns the final output of an execution: the outputs of succeeded
// sink nodes keyed by id, or the single sink's output directly.
func ResultOf(ix *workflow.Index, order []string, st Statuses, outputs map[string]any) any {
	var sinks []string
	for _, id := range order {
		if len(ix.Out[id]) == 0 {
			if s := st[id]; s != nil && s.State == NodeSucceeded {
				sinks = append(sinks, id)
			}
		}
	}
	switch len(sinks) {
	case 0:
		return nil
	case 1:
		return outputs[sinks[0]]
	}
	m := make(map[string]any, len(sinks))
	for _, id := range sinks {
		m[id] = outputs[id]
	}
	return m
}
