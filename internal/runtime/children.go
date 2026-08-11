package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
)

// newExec describes an execution to insert.
type newExec struct {
	WorkspaceID string
	WorkflowID  string
	VersionID   string
	Kind        string
	TriggerType string
	Trigger     any
	StartNode   string
	Ctx         execContext
	Parent      *string
	ParentNode  string
	ItemIndex   *int
	Depth       int
	ReplayOf    *string
	ReplaySrc   string
	Idem        string
	CreatedBy   string
	Deadline    *time.Duration
	// Seed pre-populates node results (replay from a node).
	Seed map[string]SeedNode
}

// SeedNode is a node result carried into a replayed execution.
type SeedNode struct {
	State  engine.NodeState
	Branch string
	Output any
}

// createExecution inserts rows for a new execution and returns its locked run.
func (rt *Runtime) createExecution(ctx context.Context, tx *persistence.Tx, ne newExec) (*run, error) {
	tmp := &Execution{VersionID: ne.VersionID, Kind: ne.Kind, ParentNode: ne.ParentNode}
	gi, err := rt.graphFor(ctx, tx, tmp)
	if err != nil {
		return nil, err
	}
	var idem, createdBy, replayOf any
	if ne.Idem != "" {
		idem = ne.Idem
	}
	if ne.CreatedBy != "" {
		createdBy = ne.CreatedBy
	}
	if ne.ReplayOf != nil {
		replayOf = *ne.ReplayOf
	}
	var deadline any
	if ne.Deadline != nil && *ne.Deadline > 0 {
		deadline = time.Now().Add(*ne.Deadline)
	}
	var parentNode any
	if ne.ParentNode != "" {
		parentNode = ne.ParentNode
	}
	var replaySrc any
	if ne.ReplaySrc != "" {
		replaySrc = ne.ReplaySrc
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO executions
		(workspace_id, workflow_id, version_id, kind, trigger_type, trigger_payload, start_node, context,
		 idempotency_key, parent_execution_id, parent_node_id, item_index, depth, replay_of, replay_source_node, created_by, deadline_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id::text`,
		ne.WorkspaceID, ne.WorkflowID, ne.VersionID, ne.Kind, ne.TriggerType, marshalJSON(ne.Trigger), ne.StartNode, marshalJSON(ne.Ctx),
		idem, ne.Parent, parentNode, ne.ItemIndex, ne.Depth, replayOf, replaySrc, createdBy, deadline).Scan(&id)
	if err != nil {
		return nil, err
	}
	nodes := gi.g.Nodes
	ids, types, onErr := make([]string, len(nodes)), make([]string, len(nodes)), make([]string, len(nodes))
	for i, n := range nodes {
		ids[i], types[i], onErr[i] = n.ID, string(n.Type), n.OnError
		if onErr[i] == "" {
			onErr[i] = workflow.OnErrorFail
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_executions (execution_id, node_id, node_type, on_error)
		SELECT $1, * FROM unnest($2::text[], $3::text[], $4::text[])`, id, ids, types, onErr); err != nil {
		return nil, err
	}
	r, err := rt.newRun(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	for nid, s := range ne.Seed {
		if r.rows[nid] == nil {
			continue
		}
		row := r.rows[nid]
		row.State, row.Branch = s.State, s.Branch
		r.st[nid].State, r.st[nid].Branch = s.State, s.Branch
		now := time.Now()
		row.FinishedAt = &now
		r.dirty[nid] = true
		if s.Output != nil {
			r.setOutput(nid, s.Output)
		}
	}
	r.emit(EvExecCreated, "", 0, map[string]any{"trigger_type": ne.TriggerType, "kind": ne.Kind})
	return r, nil
}

// pickTrigger chooses the trigger node to start from.
func pickTrigger(g *workflow.Graph, want string, typ workflow.NodeType) (string, bool) {
	if want != "" {
		n, ok := g.Node(want)
		return want, ok && n.Type.IsTrigger()
	}
	ts := g.Triggers()
	for _, t := range ts {
		if t.Type == typ {
			return t.ID, true
		}
	}
	if len(ts) > 0 {
		return ts[0].ID, true
	}
	return "", false
}

// startForEach fans a foreach node out into per-item child executions.
func (r *run) startForEach(n *workflow.Node, res engine.InlineResult) error {
	if err := r.set(n.ID, engine.NodeWaiting); err != nil {
		return err
	}
	r.setInput(n.ID, map[string]any{
		"items": res.Items, "total": len(res.Items), "concurrency": res.Concurrency, "on_item_error": res.OnItemError,
	})
	r.emit(EvNodeWaiting, n.ID, 0, map[string]any{"items": len(res.Items)})
	if len(res.Items) == 0 {
		return r.succeed(n, engine.ForEachOutput([]any{}, 0), workflow.BranchDone)
	}
	// driveChildren reads the stored input, so persist it first.
	if err := r.flush(); err != nil {
		return err
	}
	_, err := r.driveChildren(n)
	return err
}

type childCounts struct{ inflight, ok, bad, total int }

func (r *run) countChildren(node string) (childCounts, error) {
	var c childCounts
	err := r.tx.QueryRow(r.ctx, `SELECT
		count(*) FILTER (WHERE status NOT IN ('succeeded','failed','cancelled')),
		count(*) FILTER (WHERE status = 'succeeded'),
		count(*) FILTER (WHERE status IN ('failed','cancelled')),
		count(*)
		FROM executions WHERE parent_execution_id=$1 AND parent_node_id=$2`, r.ex.ID, node).Scan(&c.inflight, &c.ok, &c.bad, &c.total)
	return c, err
}

// driveChildren advances a waiting foreach or sub-workflow node: it spawns
// more items within the concurrency limit and completes the node when its
// children are done. It reports whether the node changed state.
func (r *run) driveChildren(n *workflow.Node) (bool, error) {
	if n.Type == workflow.TypeSubWorkflow {
		return r.driveSub(n)
	}
	var total, conc int
	var onErr string
	if err := r.tx.QueryRow(r.ctx, `SELECT coalesce((input->>'total')::int,0), coalesce((input->>'concurrency')::int,1), coalesce(input->>'on_item_error','fail')
		FROM node_executions WHERE execution_id=$1 AND node_id=$2`, r.ex.ID, n.ID).Scan(&total, &conc, &onErr); err != nil {
		return false, err
	}
	c, err := r.countChildren(n.ID)
	if err != nil {
		return false, err
	}
	if c.bad > 0 && onErr != workflow.OnErrorContinue {
		var idx int
		var msg *string
		if err := r.tx.QueryRow(r.ctx, `SELECT item_index, error FROM executions
			WHERE parent_execution_id=$1 AND parent_node_id=$2 AND status IN ('failed','cancelled') ORDER BY item_index LIMIT 1`,
			r.ex.ID, n.ID).Scan(&idx, &msg); err != nil {
			return false, err
		}
		text := "item failed"
		if msg != nil {
			text = *msg
		}
		if err := r.cancelChildrenOf(n.ID); err != nil {
			return false, err
		}
		return true, r.failNode(n, &engine.NodeError{Code: engine.CodeChild, Message: fmt.Sprintf("item %d failed: %s", idx, text)})
	}
	spawned := c.total
	if spawned < total && c.inflight < conc {
		outer, err := r.bodyContext(n)
		if err != nil {
			return false, err
		}
		for spawned < total && c.inflight < conc {
			var item []byte
			if err := r.tx.QueryRow(r.ctx, `SELECT input->'items'->($3)::int FROM node_executions WHERE execution_id=$1 AND node_id=$2`,
				r.ex.ID, n.ID, spawned).Scan(&item); err != nil {
				return false, err
			}
			idx := spawned
			child, err := r.rt.createExecution(r.ctx, r.tx, newExec{
				WorkspaceID: r.ex.WorkspaceID, WorkflowID: r.ex.WorkflowID, VersionID: r.ex.VersionID, Kind: KindForEach,
				TriggerType: "foreach", Trigger: r.ex.TriggerPayload, StartNode: workflow.ItemInputID,
				Ctx:    execContext{Item: unmarshalAny(item), Index: idx, Nodes: outer},
				Parent: &r.ex.ID, ParentNode: n.ID, ItemIndex: &idx, Depth: r.ex.Depth,
			})
			if err != nil {
				return false, err
			}
			if err := child.advance(); err != nil {
				return false, err
			}
			if err := child.flush(); err != nil {
				return false, err
			}
			spawned++
			c.inflight++
		}
	}
	if spawned < total || c.inflight > 0 {
		return false, nil
	}
	// All items done: collect results in item order.
	rows, err := r.tx.Query(r.ctx, `SELECT status, output, error FROM executions
		WHERE parent_execution_id=$1 AND parent_node_id=$2 ORDER BY item_index`, r.ex.ID, n.ID)
	if err != nil {
		return false, err
	}
	results := make([]any, 0, total)
	failed := 0
	for rows.Next() {
		var status string
		var out []byte
		var msg *string
		if err := rows.Scan(&status, &out, &msg); err != nil {
			rows.Close()
			return false, err
		}
		if status == "succeeded" {
			results = append(results, unmarshalAny(out))
		} else {
			failed++
			m := ""
			if msg != nil {
				m = *msg
			}
			results = append(results, map[string]any{"error": m})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	return true, r.succeed(n, engine.ForEachOutput(results, failed), workflow.BranchDone)
}

// bodyContext snapshots the outer node outputs the loop body references.
func (r *run) bodyContext(n *workflow.Node) (map[string]any, error) {
	body := r.gi.ix.ForEachBody(n.ID)
	seen := map[string]bool{}
	var ids []string
	for id := range body {
		for _, ref := range r.gi.refs[id] {
			if !body[ref] && !seen[ref] {
				seen[ref] = true
				ids = append(ids, ref)
			}
		}
	}
	sort.Strings(ids)
	return r.outputs(ids)
}

func (r *run) driveSub(n *workflow.Node) (bool, error) {
	var status string
	var out []byte
	var msg *string
	err := r.tx.QueryRow(r.ctx, `SELECT status, output, error FROM executions
		WHERE parent_execution_id=$1 AND parent_node_id=$2 AND item_index=0`, r.ex.ID, n.ID).Scan(&status, &out, &msg)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, r.failNode(n, &engine.NodeError{Code: engine.CodeChild, Message: "sub-workflow execution is missing"})
	}
	if err != nil {
		return false, err
	}
	switch status {
	case "succeeded":
		return true, r.succeed(n, unmarshalAny(out), "")
	case "failed", "cancelled":
		text := status
		if msg != nil {
			text = *msg
		}
		return true, r.failNode(n, &engine.NodeError{Code: engine.CodeChild, Message: "sub-workflow " + status + ": " + text})
	}
	return false, nil
}

// startSub spawns the child execution for a sub_workflow node.
func (r *run) startSub(n *workflow.Node, res engine.InlineResult) error {
	fail := func(msg string) error {
		return r.failNode(n, &engine.NodeError{Code: engine.CodeChild, Message: msg})
	}
	if r.ex.Depth+1 > r.rt.MaxDepth {
		return fail(fmt.Sprintf("sub-workflow nesting exceeds the maximum depth of %d", r.rt.MaxDepth))
	}
	var versionID string
	var raw []byte
	err := r.tx.QueryRow(r.ctx, `SELECT v.id::text, v.graph FROM workflows w
		JOIN workflow_versions v ON v.id = w.published_version_id
		WHERE w.id::text = $1 AND w.workspace_id = $2 AND w.deleted_at IS NULL`, res.SubWorkflowID, r.ex.WorkspaceID).Scan(&versionID, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || persistence.IsInvalidText(err) {
			return fail(fmt.Sprintf("sub-workflow %q does not exist or is not published", res.SubWorkflowID))
		}
		return err
	}
	var g workflow.Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return fail("sub-workflow graph is unreadable")
	}
	g.Normalize()
	start, ok := pickTrigger(&g, "", workflow.TypeManualTrigger)
	if !ok {
		return fail("sub-workflow has no trigger")
	}
	if err := r.set(n.ID, engine.NodeWaiting); err != nil {
		return err
	}
	r.setInput(n.ID, map[string]any{"workflow_id": res.SubWorkflowID, "input": res.SubInput})
	r.emit(EvNodeWaiting, n.ID, 0, map[string]any{"workflow_id": res.SubWorkflowID})
	zero := 0
	child, err := r.rt.createExecution(r.ctx, r.tx, newExec{
		WorkspaceID: r.ex.WorkspaceID, WorkflowID: res.SubWorkflowID, VersionID: versionID, Kind: KindSubFlow,
		TriggerType: "sub_workflow", Trigger: res.SubInput, StartNode: start,
		Parent: &r.ex.ID, ParentNode: n.ID, ItemIndex: &zero, Depth: r.ex.Depth + 1,
	})
	if err != nil {
		return err
	}
	if err := child.advance(); err != nil {
		return err
	}
	return child.flush()
}

// cancelChildrenOf cancels the unfinished children spawned by one node.
func (r *run) cancelChildrenOf(node string) error {
	return r.cancelChildrenWhere(`AND parent_node_id = $2`, node)
}

func (r *run) cancelChildren() error { return r.cancelChildrenWhere(``) }

func (r *run) cancelChildrenWhere(extra string, args ...any) error {
	q := `SELECT id::text FROM executions WHERE parent_execution_id=$1 AND status NOT IN ('succeeded','failed','cancelled') ` + extra + ` ORDER BY id`
	rows, err := r.tx.Query(r.ctx, q, append([]any{r.ex.ID}, args...)...)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		child, err := r.rt.newRun(r.ctx, r.tx, id)
		if err != nil {
			return err
		}
		if err := child.terminate(engine.ExecCancelled, "parent execution ended"); err != nil {
			return err
		}
		if err := child.flush(); err != nil {
			return err
		}
	}
	return nil
}
