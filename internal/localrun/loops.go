package localrun

import (
	"context"
	"fmt"
	"sync"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/workflow"
)

// runForEach executes the extracted body once per item with bounded concurrency.
func (c *coord) runForEach(n *workflow.Node, res engine.InlineResult) {
	body := workflow.ExtractBody(c.ix.Graph, n.ID)
	results := make([]any, len(res.Items))
	errs := make([]error, len(res.Items))
	failed := 0
	var mu sync.Mutex
	sem := make(chan struct{}, res.Concurrency)
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	var wg sync.WaitGroup
	// Body nodes may read any outer node output; pass a snapshot.
	c.snapMu.Lock()
	snap := make(map[string]any, len(c.ectx.Nodes))
	for k, v := range c.ectx.Nodes {
		snap[k] = v
	}
	c.snapMu.Unlock()
loop:
	for i, item := range res.Items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(i int, item any) {
			defer wg.Done()
			defer func() { <-sem }()
			g := body // copy of the struct; nodes are read-only
			r, err := c.r.run(ctx, &g, runInput{
				trigger: c.in.trigger, start: workflow.ItemInputID, item: item, index: i, inLoop: true, nodes: snap, depth: c.in.depth,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs[i], failed = err, failed+1
			case r.Status != engine.ExecSucceeded:
				errs[i], failed = fmt.Errorf("iteration %d %s: %s", i, r.Status, r.Error), failed+1
				results[i] = map[string]any{"error": r.Error}
				if res.OnItemError != workflow.OnErrorContinue {
					cancel()
				}
			default:
				results[i] = r.Output
			}
		}(i, item)
	}
	wg.Wait()
	if c.ctx.Err() != nil {
		return
	}
	if failed > 0 && res.OnItemError != workflow.OnErrorContinue {
		var first error
		for _, e := range errs {
			if e != nil {
				first = e
				break
			}
		}
		c.send(event{id: n.ID, kind: "child", err: &engine.NodeError{Code: engine.CodeChild, Message: first.Error()}})
		return
	}
	c.send(event{id: n.ID, kind: "child", out: engine.ForEachOutput(results, failed)})
}

func (c *coord) runSub(n *workflow.Node, res engine.InlineResult) {
	fail := func(msg string) {
		c.send(event{id: n.ID, kind: "child", err: &engine.NodeError{Code: engine.CodeChild, Message: msg}})
	}
	if c.in.depth+1 > c.r.MaxDepth {
		fail(fmt.Sprintf("sub-workflow nesting exceeds the maximum depth of %d", c.r.MaxDepth))
		return
	}
	if c.r.Workflows == nil {
		fail("sub-workflows are not available")
		return
	}
	g, err := c.r.Workflows(res.SubWorkflowID)
	if err != nil {
		fail(err.Error())
		return
	}
	r, err := c.r.run(c.ctx, g, runInput{trigger: res.SubInput, depth: c.in.depth + 1})
	switch {
	case err != nil:
		fail(err.Error())
	case r.Status != engine.ExecSucceeded:
		fail("sub-workflow " + string(r.Status) + ": " + r.Error)
	default:
		c.send(event{id: n.ID, kind: "child", out: r.Output})
	}
}
