package runtime

import (
	"fmt"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

// advance drives the execution as far as it can go without waiting on
// anything external: it resolves edges, runs inline nodes, enqueues tasks and
// spawns children, then settles the execution status.
func (r *run) advance() error {
	switch {
	case r.ex.Status.Terminal():
		return nil
	case r.ex.Status == engine.ExecCancelling:
		return r.settleCancelling()
	}
	if r.ex.Status == engine.ExecCreated {
		if err := r.begin(); err != nil {
			return err
		}
	}
	for i := 0; i < 1_000_000; i++ {
		progressed, err := r.step()
		if err != nil {
			return err
		}
		if r.done {
			return nil
		}
		if out := engine.Assess(r.st); out.Done {
			return r.finishNormally()
		}
		if !progressed {
			break
		}
	}
	r.updateStatus()
	return nil
}

func (r *run) begin() error {
	now := time.Now()
	r.ex.Status = engine.ExecRunning
	r.ex.StartedAt = &now
	r.exDirty = true
	r.emit(EvExecStarted, "", 0, nil)
	for _, n := range r.gi.g.Nodes {
		if !n.Type.IsTrigger() {
			continue
		}
		cur := r.rows[n.ID]
		if cur == nil || cur.State != engine.NodePending {
			continue // seeded by a replay
		}
		if n.ID == r.ex.StartNode {
			if err := r.set(n.ID, engine.NodeReady); err != nil {
				return err
			}
		} else if err := r.skip(n.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) skip(id string) error {
	if err := r.set(id, engine.NodeSkipped); err != nil {
		return err
	}
	r.emit(EvNodeSkipped, id, 0, nil)
	return nil
}

// step performs one round of resolution and dispatch.
func (r *run) step() (bool, error) {
	progressed := false
	for _, id := range r.gi.order {
		n := r.gi.ix.Nodes[id]
		if r.st[id].State != engine.NodeWaiting || (n.Type != workflow.TypeForEach && n.Type != workflow.TypeSubWorkflow) {
			continue
		}
		changed, err := r.driveChildren(n)
		if err != nil {
			return false, err
		}
		if r.done {
			return true, nil
		}
		progressed = progressed || changed
	}
	d := engine.Resolve(r.gi.ix, r.gi.order, r.st)
	for _, id := range d.Skipped {
		if err := r.skip(id); err != nil {
			return false, err
		}
		progressed = true
	}
	for _, id := range d.Ready {
		if err := r.set(id, engine.NodeReady); err != nil {
			return false, err
		}
	}
	for _, id := range r.gi.order {
		if r.st[id].State != engine.NodeReady {
			continue
		}
		if err := r.dispatch(id); err != nil {
			return false, err
		}
		progressed = true
		if r.done {
			return true, nil
		}
	}
	return progressed, nil
}

// updateStatus toggles running/waiting based on whether anything is in flight.
func (r *run) updateStatus() {
	want := engine.ExecRunning
	if engine.Assess(r.st).Idle {
		want = engine.ExecWaiting
	}
	if r.ex.Status != want && engine.CanExecTransition(r.ex.Status, want) {
		r.ex.Status = want
		r.exDirty = true
	}
}

func (r *run) dispatch(id string) error {
	n := r.gi.ix.Nodes[id]
	if !n.Type.IsInline() {
		return r.enqueue(n, 1)
	}
	refs := r.gi.refs[id]
	var sourceIDs []string
	if n.Type == workflow.TypeMerge {
		sourceIDs = engine.ActiveSources(r.gi.ix, id, r.st)
		refs = append(append([]string{}, refs...), sourceIDs...)
	}
	ectx, err := r.engineCtx(refs)
	if err != nil {
		return err
	}
	var sources map[string]any
	if n.Type == workflow.TypeMerge {
		sources = make(map[string]any, len(sourceIDs))
		for _, s := range sourceIDs {
			sources[s] = ectx.Nodes[s]
		}
	}
	res := engine.EvalInline(n, ectx, ectx.Env(r.secretFn(), nil, r.rt.Now), r.rt.Now(), sources)
	switch res.Kind {
	case engine.InlineDone:
		return r.succeed(n, res.Output, res.Branch)
	case engine.InlineFail:
		return r.failNode(n, res.Err)
	case engine.InlineStop:
		if err := r.succeed(n, res.Output, ""); err != nil {
			return err
		}
		if res.StopStatus == "failed" {
			return r.terminate(engine.ExecFailed, expressions.ToString(res.Output.(map[string]any)["message"]))
		}
		return r.terminate(engine.ExecSucceeded, "")
	case engine.InlineWait:
		if err := r.set(id, engine.NodeWaiting); err != nil {
			return err
		}
		row := r.rows[id]
		row.WakeAt = &res.WakeAt
		r.setOutput(id, res.Output)
		r.emit(EvNodeWaiting, id, 0, map[string]any{"wake_at": res.WakeAt.UTC().Format(time.RFC3339Nano)})
		return nil
	case engine.InlineForEach:
		return r.startForEach(n, res)
	case engine.InlineSub:
		return r.startSub(n, res)
	}
	return fmt.Errorf("unhandled inline result for %s", id)
}

// succeed completes a node with an output.
func (r *run) succeed(n *workflow.Node, out any, branch string) error {
	if size := len(marshalJSON(out)); size > r.rt.MaxOutputBytes {
		return r.failNode(n, &engine.NodeError{Code: engine.CodeInternal,
			Message: fmt.Sprintf("node output is %d bytes; the limit is %d", size, r.rt.MaxOutputBytes)})
	}
	row := r.rows[n.ID]
	if row.State == engine.NodeReady && row.StartedAt == nil {
		now := time.Now()
		row.StartedAt = &now
	}
	if err := r.set(n.ID, engine.NodeSucceeded); err != nil {
		return err
	}
	row.Branch = branch
	row.WakeAt = nil
	row.Error = nil
	r.st[n.ID].Branch = branch
	r.setOutput(n.ID, out)
	r.emit(EvNodeSucceeded, n.ID, row.Attempt, nil)
	return nil
}

// failNode applies the node's on_error policy. A failure without
// continue-on-error terminates the execution.
func (r *run) failNode(n *workflow.Node, ne *engine.NodeError) error {
	row := r.rows[n.ID]
	if row.State == engine.NodeReady && row.StartedAt == nil {
		now := time.Now()
		row.StartedAt = &now
	}
	if err := r.set(n.ID, engine.NodeFailed); err != nil {
		return err
	}
	row.Error = ne
	row.WakeAt = nil
	r.emit(EvNodeFailed, n.ID, row.Attempt, map[string]any{"error": ne})
	if n.OnError == workflow.OnErrorContinue {
		row.Branch = engine.ContinueBranch(n.Type)
		r.st[n.ID].Branch = row.Branch
		r.setOutput(n.ID, map[string]any{"error": ne.Message, "code": ne.Code})
		return nil
	}
	return r.terminate(engine.ExecFailed, fmt.Sprintf("node %s failed: %s", n.ID, ne.Message))
}

// finishNormally completes an execution whose nodes are all terminal.
func (r *run) finishNormally() error {
	return r.terminate(engine.ExecSucceeded, "")
}

// terminate finishes the execution: it cancels whatever is still in flight,
// computes the result and records the terminal status.
func (r *run) terminate(status engine.ExecState, msg string) error {
	if r.done {
		return nil
	}
	for id, s := range r.st {
		if s.State.Terminal() {
			continue
		}
		r.force(id, engine.NodeCancelled)
		r.emit(EvNodeCancelled, id, r.rows[id].Attempt, nil)
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE tasks SET status='cancelled', finished_at=now()
		WHERE execution_id=$1 AND status IN ('queued','leased')`, r.ex.ID); err != nil {
		return err
	}
	if err := r.cancelChildren(); err != nil {
		return err
	}
	if status == engine.ExecSucceeded {
		var sinks []string
		for _, id := range r.gi.order {
			if len(r.gi.ix.Out[id]) == 0 && r.st[id].State == engine.NodeSucceeded {
				sinks = append(sinks, id)
			}
		}
		outs, err := r.outputs(sinks)
		if err != nil {
			return err
		}
		r.ex.Output = engine.ResultOf(r.gi.ix, r.gi.order, r.st, outs)
	}
	now := time.Now()
	r.ex.Status = status
	r.ex.Error = msg
	r.ex.FinishedAt = &now
	r.exDirty = true
	r.done = true
	typ := map[engine.ExecState]string{
		engine.ExecSucceeded: EvExecSucceeded, engine.ExecFailed: EvExecFailed, engine.ExecCancelled: EvExecCancelled,
	}[status]
	data := map[string]any{"duration_ms": now.Sub(r.ex.CreatedAt).Milliseconds()}
	if msg != "" {
		data["error"] = msg
	}
	r.emit(typ, "", 0, data)
	if r.ex.ParentExecution != nil && !r.rt.DisableParentNotify {
		parent := *r.ex.ParentExecution
		ctx, rt := r.ctx, r.rt
		r.tx.AfterCommit(func() {
			if err := rt.AdvanceParent(ctx, parent); err != nil {
				rt.Log.Warn("advance parent failed; the sweeper will retry", "parent", parent, "err", err)
			}
		})
	}
	return nil
}

// cancel handles a user cancellation request.
func (r *run) cancel(reason string) error {
	if r.ex.Status.Terminal() {
		return ErrTerminal
	}
	leased, err := r.leasedNodes()
	if err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE tasks SET status='cancelled', finished_at=now()
		WHERE execution_id=$1 AND status='queued'`, r.ex.ID); err != nil {
		return err
	}
	running := 0
	for id, s := range r.st {
		switch {
		case s.State.Terminal():
		case s.State == engine.NodeRunning && leased[id]:
			running++
		default:
			r.force(id, engine.NodeCancelled)
			r.emit(EvNodeCancelled, id, r.rows[id].Attempt, nil)
		}
	}
	if err := r.cancelChildren(); err != nil {
		return err
	}
	if running == 0 {
		return r.terminate(engine.ExecCancelled, reason)
	}
	r.ex.Status = engine.ExecCancelling
	r.exDirty = true
	r.emit(EvExecCancelReq, "", 0, map[string]any{"reason": reason})
	return nil
}

// settleCancelling finishes a cancelling execution once no node is running.
func (r *run) settleCancelling() error {
	leased, err := r.leasedNodes()
	if err != nil {
		return err
	}
	for id, s := range r.st {
		if s.State.Terminal() {
			continue
		}
		if s.State == engine.NodeRunning && leased[id] {
			return nil
		}
		r.force(id, engine.NodeCancelled)
		r.emit(EvNodeCancelled, id, r.rows[id].Attempt, nil)
	}
	return r.terminate(engine.ExecCancelled, "cancelled")
}

func (r *run) leasedNodes() (map[string]bool, error) {
	rows, err := r.tx.Query(r.ctx, `SELECT node_id FROM tasks WHERE execution_id=$1 AND status='leased'`, r.ex.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
