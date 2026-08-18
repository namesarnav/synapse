package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/workflow"
)

// ErrNotReplayable means the execution or node cannot be replayed.
var ErrNotReplayable = errors.New("not replayable")

// ReplayParams describes a replay of a finished root execution.
type ReplayParams struct {
	WorkspaceID    string
	ExecutionID    string
	FromNode       string // empty replays the whole execution
	CreatedBy      string
	IdempotencyKey string
}

// Replay starts a new execution derived from a finished one. The original is
// never modified. It runs the same pinned workflow version with the same
// trigger payload. With FromNode, outputs of nodes that are not downstream of
// it are reused and only that node and everything after it run again.
func (rt *Runtime) Replay(ctx context.Context, p ReplayParams) (*StartResult, error) {
	orig, err := rt.Get(ctx, p.WorkspaceID, p.ExecutionID)
	if err != nil {
		return nil, err
	}
	if orig.Kind != KindRoot {
		return nil, fmt.Errorf("%w: only root executions can be replayed", ErrNotReplayable)
	}
	if !orig.Status.Terminal() {
		return nil, fmt.Errorf("%w: execution is still %s", ErrNotReplayable, orig.Status)
	}
	sp := StartParams{
		WorkspaceID: orig.WorkspaceID, WorkflowID: orig.WorkflowID, VersionID: orig.VersionID,
		TriggerType: "replay", Trigger: orig.TriggerPayload, StartNode: orig.StartNode,
		ReplayOf: orig.ID, ReplaySource: p.FromNode, CreatedBy: p.CreatedBy, IdempotencyKey: p.IdempotencyKey,
	}
	if p.FromNode != "" && p.FromNode != orig.StartNode {
		if sp.Seed, err = rt.replaySeed(ctx, orig, p.FromNode); err != nil {
			return nil, err
		}
	} else {
		sp.ReplaySource = ""
	}
	return rt.Start(ctx, sp)
}

// replaySeed picks the node results a partial replay reuses.
func (rt *Runtime) replaySeed(ctx context.Context, orig *Execution, from string) (map[string]SeedNode, error) {
	gi, err := rt.graphFor(ctx, rt.DB.Pool, orig)
	if err != nil {
		return nil, err
	}
	node := gi.ix.Nodes[from]
	if node == nil {
		return nil, fmt.Errorf("%w: node %q does not exist in version %d", ErrNotReplayable, from, orig.Version)
	}
	if node.Type.IsTrigger() {
		return nil, fmt.Errorf("%w: replay from a trigger is a full replay", ErrNotReplayable)
	}
	for _, n := range gi.g.Nodes {
		if n.Type == workflow.TypeForEach && gi.ix.ForEachBody(n.ID)[from] {
			return nil, fmt.Errorf("%w: node %q is inside the body of foreach %q", ErrNotReplayable, from, n.ID)
		}
	}
	rows, err := rt.Nodes(ctx, orig.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]NodeExec, len(rows))
	for _, n := range rows {
		byID[n.NodeID] = n
	}
	rerun := gi.ix.Descendants(from)
	rerun[from] = true

	// Everything the node depends on directly must have settled in the original.
	for _, e := range gi.ix.In[from] {
		if !reusable(byID[e.Source], gi.ix.Nodes[e.Source]) {
			return nil, fmt.Errorf("%w: upstream node %q did not complete (state %s)", ErrNotReplayable, e.Source, byID[e.Source].State)
		}
	}
	seed := map[string]SeedNode{}
	for id, n := range byID {
		if rerun[id] || !reusable(n, gi.ix.Nodes[id]) {
			continue
		}
		seed[id] = SeedNode{State: n.State, Branch: n.Branch, Output: n.Output}
	}
	return seed, nil
}

// reusable reports whether a node's recorded result can stand in for a re-run.
func reusable(n NodeExec, def *workflow.Node) bool {
	switch n.State {
	case engine.NodeSucceeded, engine.NodeSkipped:
		return true
	case engine.NodeFailed:
		return def != nil && def.OnError == workflow.OnErrorContinue
	}
	return false
}
