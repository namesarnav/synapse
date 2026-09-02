package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
)

// run is the in-transaction working copy of one execution. It is created after
// the execution row is locked and discarded when the transaction ends.
type run struct {
	rt  *Runtime
	ctx context.Context
	tx  *persistence.Tx
	ex  *Execution
	gi  *graphInfo

	st   engine.Statuses
	rows map[string]*NodeExec
	// dirty tracks rows to persist; outSet/inSet say whether output/input changed.
	dirty  map[string]bool
	outSet map[string]bool
	inSet  map[string]bool

	outs   map[string]any // outputs loaded or produced in this run
	loaded map[string]bool

	events  []Event
	exDirty bool
	done    bool // execution reached a terminal state in this run

	secrets    map[string]string
	secretsErr error
	secretsOK  bool
}

const execCols = `e.id::text, e.workspace_id::text, e.workflow_id::text, e.version_id::text, v.version, e.kind, e.status,
	e.trigger_type, e.trigger_payload, e.start_node, e.context, e.output, coalesce(e.error,''),
	e.parent_execution_id::text, coalesce(e.parent_node_id,''), e.item_index, e.depth, e.replay_of::text,
	coalesce(e.replay_source_node,''), e.idempotency_key, e.deadline_at, e.created_at, e.started_at, e.finished_at, e.traceparent`

func scanExecution(row pgx.Row) (*Execution, error) {
	var ex Execution
	var status string
	var trig, ctxb, out []byte
	err := row.Scan(&ex.ID, &ex.WorkspaceID, &ex.WorkflowID, &ex.VersionID, &ex.Version, &ex.Kind, &status,
		&ex.TriggerType, &trig, &ex.StartNode, &ctxb, &out, &ex.Error,
		&ex.ParentExecution, &ex.ParentNode, &ex.ItemIndex, &ex.Depth, &ex.ReplayOf,
		&ex.ReplaySourceNode, &ex.IdempotencyKey, &ex.DeadlineAt, &ex.CreatedAt, &ex.StartedAt, &ex.FinishedAt, &ex.Traceparent)
	if err != nil {
		return nil, persistence.NotFound(err)
	}
	ex.Status = engine.ExecState(status)
	ex.TriggerPayload = unmarshalAny(trig)
	ex.Output = unmarshalAny(out)
	if len(ctxb) > 0 {
		_ = json.Unmarshal(ctxb, &ex.ctx)
	}
	return &ex, nil
}

// lockExecution loads an execution and takes its row lock.
func lockExecution(ctx context.Context, tx persistence.Querier, id string) (*Execution, error) {
	return scanExecution(tx.QueryRow(ctx, `SELECT `+execCols+`
		FROM executions e JOIN workflow_versions v ON v.id = e.version_id
		WHERE e.id = $1 FOR UPDATE OF e`, id))
}

// graphFor returns the (cached) graph an execution runs.
func (rt *Runtime) graphFor(ctx context.Context, q persistence.Querier, ex *Execution) (*graphInfo, error) {
	key := ex.VersionID
	if ex.Kind == KindForEach {
		key += "#" + ex.ParentNode
	}
	if v, ok := rt.graphs.Load(key); ok {
		return v.(*graphInfo), nil
	}
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT graph FROM workflow_versions WHERE id = $1`, ex.VersionID).Scan(&raw); err != nil {
		return nil, persistence.NotFound(err)
	}
	var g workflow.Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("decode version graph: %w", err)
	}
	g.Normalize()
	if ex.Kind == KindForEach {
		g = workflow.ExtractBody(&g, ex.ParentNode)
	}
	gi, err := buildGraphInfo(g)
	if err != nil {
		return nil, err
	}
	rt.graphs.Store(key, gi)
	return gi, nil
}

// newRun locks the execution and loads its node statuses.
func (rt *Runtime) newRun(ctx context.Context, tx *persistence.Tx, id string) (*run, error) {
	ex, err := lockExecution(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return rt.loadRun(ctx, tx, ex)
}

func (rt *Runtime) loadRun(ctx context.Context, tx *persistence.Tx, ex *Execution) (*run, error) {
	gi, err := rt.graphFor(ctx, tx, ex)
	if err != nil {
		return nil, err
	}
	r := &run{
		rt: rt, ctx: ctx, tx: tx, ex: ex, gi: gi,
		st: engine.Statuses{}, rows: map[string]*NodeExec{},
		dirty: map[string]bool{}, outSet: map[string]bool{}, inSet: map[string]bool{},
		outs: map[string]any{}, loaded: map[string]bool{},
	}
	rows, err := tx.Query(ctx, `SELECT node_id, node_type, state, branch, on_error, attempt, error, wake_at, worker_id, started_at, finished_at
		FROM node_executions WHERE execution_id = $1`, ex.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n NodeExec
		var state, onErr string
		var errb []byte
		var worker *string
		if err := rows.Scan(&n.NodeID, &n.NodeType, &state, &n.Branch, &onErr, &n.Attempt, &errb, &n.WakeAt, &worker, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, err
		}
		n.State = engine.NodeState(state)
		if worker != nil {
			n.WorkerID = *worker
		}
		if len(errb) > 0 && string(errb) != "null" {
			var ne engine.NodeError
			if json.Unmarshal(errb, &ne) == nil {
				n.Error = &ne
			}
		}
		r.rows[n.NodeID] = &n
		r.st[n.NodeID] = &engine.Status{State: n.State, Branch: n.Branch, OnError: onErr}
	}
	return r, rows.Err()
}

// mutate runs fn with the execution locked, then advances it (when advance is
// set) and persists everything in the same transaction.
func (rt *Runtime) mutate(ctx context.Context, id string, advance bool, fn func(r *run) error) error {
	return rt.DB.InTx(ctx, func(tx *persistence.Tx) error {
		r, err := rt.newRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if fn != nil {
			if err := fn(r); err != nil {
				return err
			}
		}
		if advance {
			if err := r.advance(); err != nil {
				return err
			}
		}
		return r.flush()
	})
}

// Advance re-evaluates an execution; it is idempotent and safe from any process.
func (rt *Runtime) Advance(ctx context.Context, id string) error {
	err := rt.mutate(ctx, id, true, nil)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil
	}
	return err
}

// state helpers -------------------------------------------------------------

func (r *run) emit(typ, node string, attempt int, data map[string]any) {
	r.events = append(r.events, Event{
		ExecutionID: r.ex.ID, WorkflowID: r.ex.WorkflowID, WorkspaceID: r.ex.WorkspaceID,
		Type: typ, NodeID: node, Attempt: attempt, Data: data,
	})
}

// set moves a node through a validated transition.
func (r *run) set(id string, to engine.NodeState) error {
	n := r.rows[id]
	if n == nil {
		return fmt.Errorf("unknown node %q", id)
	}
	if n.State == to {
		return nil
	}
	if err := engine.CheckNodeTransition(n.State, to); err != nil {
		return fmt.Errorf("node %s: %w", id, err)
	}
	r.force(id, to)
	return nil
}

// force sets the state without validating; used for cancellation sweeps.
func (r *run) force(id string, to engine.NodeState) {
	n := r.rows[id]
	n.State = to
	r.st[id].State = to
	now := time.Now()
	if to.Terminal() && n.FinishedAt == nil {
		n.FinishedAt = &now
	}
	r.dirty[id] = true
}

func (r *run) setOutput(id string, out any) {
	r.outs[id] = out
	r.loaded[id] = true
	r.outSet[id] = true
}

func (r *run) setInput(id string, in any) {
	r.rows[id].Input = in
	r.inSet[id] = true
	r.dirty[id] = true
}

// outputs returns the outputs of the given nodes, loading them on demand.
// Nodes not run by this execution fall back to the inherited context.
func (r *run) outputs(ids []string) (map[string]any, error) {
	var need []string
	for _, id := range ids {
		if !r.loaded[id] && r.rows[id] != nil {
			need = append(need, id)
		}
	}
	if len(need) > 0 {
		rows, err := r.tx.Query(r.ctx, `SELECT node_id, output FROM node_executions
			WHERE execution_id = $1 AND node_id = ANY($2) AND state IN ('succeeded','failed')`, r.ex.ID, need)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var b []byte
			if err := rows.Scan(&id, &b); err != nil {
				rows.Close()
				return nil, err
			}
			r.outs[id] = unmarshalAny(b)
			r.loaded[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, id := range need {
			r.loaded[id] = true
		}
	}
	out := make(map[string]any, len(ids))
	for _, id := range ids {
		if r.rows[id] != nil {
			if v, ok := r.outs[id]; ok {
				out[id] = v
			}
		} else if v, ok := r.ex.ctx.Nodes[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// engineCtx builds the expression context for a node.
func (r *run) engineCtx(ids []string) (*engine.Context, error) {
	nodes, err := r.outputs(ids)
	if err != nil {
		return nil, err
	}
	return &engine.Context{
		ExecutionID: r.ex.ID, WorkflowID: r.ex.WorkflowID, Version: r.ex.Version,
		Trigger: r.ex.TriggerPayload, Nodes: nodes,
		Item: r.ex.ctx.Item, Index: r.ex.ctx.Index, InLoop: r.ex.Kind == KindForEach,
	}, nil
}

func (r *run) secretFn() func(string) (string, bool) {
	return func(name string) (string, bool) {
		if r.rt.Secrets == nil {
			return "", false
		}
		if !r.secretsOK {
			r.secrets, r.secretsErr = r.rt.Secrets.Load(r.ctx, r.ex.WorkspaceID)
			r.secretsOK = true
		}
		v, ok := r.secrets[name]
		return v, ok
	}
}

// flush persists node rows, the execution row and events.
func (r *run) flush() error {
	b := &pgx.Batch{}
	for id := range r.dirty {
		n := r.rows[id]
		errb := []byte(nil)
		if n.Error != nil {
			errb = marshalJSON(n.Error)
		}
		var worker any
		if n.WorkerID != "" {
			worker = n.WorkerID
		}
		b.Queue(`UPDATE node_executions SET state=$3, branch=$4, attempt=$5, error=$6, wake_at=$7, worker_id=$8,
			started_at=$9, finished_at=$10, updated_at=now() WHERE execution_id=$1 AND node_id=$2`,
			r.ex.ID, id, string(n.State), n.Branch, n.Attempt, errb, n.WakeAt, worker, n.StartedAt, n.FinishedAt)
		if r.outSet[id] {
			b.Queue(`UPDATE node_executions SET output=$3 WHERE execution_id=$1 AND node_id=$2`, r.ex.ID, id, marshalJSON(r.outs[id]))
		}
		if r.inSet[id] {
			b.Queue(`UPDATE node_executions SET input=$3 WHERE execution_id=$1 AND node_id=$2`, r.ex.ID, id, marshalJSON(n.Input))
		}
	}
	if r.exDirty {
		var out []byte
		if r.ex.Output != nil {
			out = marshalJSON(r.ex.Output)
		}
		b.Queue(`UPDATE executions SET status=$2, output=$3, error=NULLIF($4,''), started_at=$5, finished_at=$6, updated_at=now() WHERE id=$1`,
			r.ex.ID, string(r.ex.Status), out, r.ex.Error, r.ex.StartedAt, r.ex.FinishedAt)
	}
	if b.Len() > 0 {
		br := r.tx.SendBatch(r.ctx, b)
		for i := 0; i < b.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
	}
	r.dirty, r.outSet, r.inSet, r.exDirty = map[string]bool{}, map[string]bool{}, map[string]bool{}, false
	return r.flushEvents()
}

func (r *run) flushEvents() error {
	if len(r.events) == 0 {
		return nil
	}
	evs := r.events
	r.events = nil
	b := &pgx.Batch{}
	for _, e := range evs {
		b.Queue(`INSERT INTO execution_events (execution_id, type, node_id, attempt, data) VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
			e.ExecutionID, e.Type, e.NodeID, e.Attempt, marshalJSON(nonNil(e.Data)))
	}
	br := r.tx.SendBatch(r.ctx, b)
	for i := range evs {
		if err := br.QueryRow().Scan(&evs[i].ID, &evs[i].CreatedAt); err != nil {
			_ = br.Close()
			return err
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	for i := range evs {
		evs[i].WorkspaceID, evs[i].WorkflowID = r.ex.WorkspaceID, r.ex.WorkflowID
	}
	if r.rt.OnEvents != nil {
		fn := r.rt.OnEvents
		r.tx.AfterCommit(func() { fn(evs) })
	}
	return nil
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
