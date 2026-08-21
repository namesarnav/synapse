package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/tracing"
	"github.com/namesarnav/synapse/internal/workflow"
)

// StartParams describes a new root execution.
type StartParams struct {
	WorkspaceID string
	WorkflowID  string
	// VersionID pins a version; empty means the published one.
	VersionID      string
	TriggerType    string // manual | webhook | schedule | replay
	Trigger        any
	StartNode      string
	IdempotencyKey string
	CreatedBy      string
	ReplayOf       string
	ReplaySource   string
	Seed           map[string]SeedNode
}

// StartResult is the outcome of Start.
type StartResult struct {
	Execution *Execution
	Duplicate bool
}

var triggerTypes = map[string]workflow.NodeType{
	"manual": workflow.TypeManualTrigger, "webhook": workflow.TypeWebhookTrigger, "schedule": workflow.TypeScheduleTrigger,
}

// Start creates and begins an execution. With an idempotency key, repeated
// calls return the original execution.
func (rt *Runtime) Start(ctx context.Context, p StartParams) (*StartResult, error) {
	ctx, span := tracing.Start(ctx, "execution.start", attribute.String("synapse.workflow_id", p.WorkflowID),
		attribute.String("synapse.trigger_type", p.TriggerType))
	res, err := rt.start(ctx, p)
	if res != nil && res.Execution != nil {
		span.SetAttributes(attribute.String("synapse.execution_id", res.Execution.ID), attribute.Bool("synapse.duplicate", res.Duplicate))
	}
	tracing.End(span, err)
	return res, err
}

func (rt *Runtime) start(ctx context.Context, p StartParams) (*StartResult, error) {
	if p.TriggerType == "" {
		p.TriggerType = "manual"
	}
	if rt.MaxQueueDepth > 0 {
		if n, err := rt.QueueDepth(ctx, rt.MaxQueueDepth+1); err == nil && n >= rt.MaxQueueDepth {
			return nil, ErrQueueFull
		}
	}
	for attempt := 0; ; attempt++ {
		var out *StartResult
		err := rt.DB.InTx(ctx, func(tx *persistence.Tx) error {
			var err error
			out, err = rt.startTx(ctx, tx, p)
			return err
		})
		if err != nil && p.IdempotencyKey != "" && persistence.IsUniqueViolation(err, "executions_idem_unique") && attempt < 3 {
			if ex, gerr := rt.byIdempotencyKey(ctx, p.WorkflowID, p.IdempotencyKey); gerr == nil {
				return &StartResult{Execution: ex, Duplicate: true}, nil
			}
			continue
		}
		return out, err
	}
}

func (rt *Runtime) byIdempotencyKey(ctx context.Context, workflowID, key string) (*Execution, error) {
	return scanExecution(rt.DB.Pool.QueryRow(ctx, `SELECT `+execCols+` FROM executions e
		JOIN workflow_versions v ON v.id = e.version_id WHERE e.workflow_id=$1 AND e.idempotency_key=$2 AND e.kind='root'`, workflowID, key))
}

func (rt *Runtime) startTx(ctx context.Context, tx *persistence.Tx, p StartParams) (*StartResult, error) {
	if p.IdempotencyKey != "" {
		if ex, err := scanExecution(tx.QueryRow(ctx, `SELECT `+execCols+` FROM executions e
			JOIN workflow_versions v ON v.id = e.version_id WHERE e.workflow_id=$1 AND e.idempotency_key=$2 AND e.kind='root'`,
			p.WorkflowID, p.IdempotencyKey)); err == nil {
			return &StartResult{Execution: ex, Duplicate: true}, nil
		}
	}
	var versionID string
	var raw []byte
	var err error
	if p.VersionID != "" {
		err = tx.QueryRow(ctx, `SELECT v.id::text, v.graph FROM workflow_versions v JOIN workflows w ON w.id = v.workflow_id
			WHERE v.id::text=$1 AND w.id::text=$2 AND w.workspace_id=$3 AND w.deleted_at IS NULL`, p.VersionID, p.WorkflowID, p.WorkspaceID).Scan(&versionID, &raw)
	} else {
		err = tx.QueryRow(ctx, `SELECT v.id::text, v.graph FROM workflows w JOIN workflow_versions v ON v.id = w.published_version_id
			WHERE w.id::text=$1 AND w.workspace_id=$2 AND w.deleted_at IS NULL`, p.WorkflowID, p.WorkspaceID).Scan(&versionID, &raw)
	}
	if errors.Is(err, pgx.ErrNoRows) || persistence.IsInvalidText(err) {
		return nil, ErrNotPublished
	}
	if err != nil {
		return nil, err
	}
	var g workflow.Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("decode version graph: %w", err)
	}
	g.Normalize()
	start, ok := pickTrigger(&g, p.StartNode, triggerTypes[p.TriggerType])
	if !ok {
		return nil, fmt.Errorf("%w: start node %q is not a trigger", ErrInvalidTrigger, p.StartNode)
	}
	ne := newExec{
		WorkspaceID: p.WorkspaceID, WorkflowID: p.WorkflowID, VersionID: versionID, Kind: KindRoot,
		TriggerType: p.TriggerType, Trigger: p.Trigger, StartNode: start, Idem: p.IdempotencyKey,
		CreatedBy: p.CreatedBy, ReplaySrc: p.ReplaySource, Seed: p.Seed,
	}
	if p.ReplayOf != "" {
		ne.ReplayOf = &p.ReplayOf
	}
	if rt.WorkflowTimeout > 0 {
		d := rt.WorkflowTimeout
		ne.Deadline = &d
	}
	r, err := rt.createExecution(ctx, tx, ne)
	if err != nil {
		return nil, err
	}
	if err := r.advance(); err != nil {
		return nil, err
	}
	if err := r.flush(); err != nil {
		return nil, err
	}
	return &StartResult{Execution: r.ex}, nil
}

// Cancel requests cancellation of an execution and its children.
func (rt *Runtime) Cancel(ctx context.Context, id, reason string) error {
	err := rt.mutate(ctx, id, false, func(r *run) error { return r.cancel(reason) })
	return err
}

// AdvanceParent re-advances a parent after one of its children finished.
func (rt *Runtime) AdvanceParent(ctx context.Context, parentID string) error {
	err := rt.DB.InTx(ctx, func(tx *persistence.Tx) error {
		r, err := rt.newRun(ctx, tx, parentID)
		if err != nil {
			return err
		}
		// Mark first: a child finishing after this point stays unacknowledged
		// and is picked up by the sweeper, never lost.
		if _, err := tx.Exec(ctx, `UPDATE executions SET notified_at = now()
			WHERE parent_execution_id=$1 AND finished_at IS NOT NULL AND notified_at IS NULL`, parentID); err != nil {
			return err
		}
		if err := r.advance(); err != nil {
			return err
		}
		return r.flush()
	})
	if errors.Is(err, persistence.ErrNotFound) {
		return nil
	}
	return err
}

// Reads ---------------------------------------------------------------------

// Get returns an execution scoped to a workspace.
func (rt *Runtime) Get(ctx context.Context, workspaceID, id string) (*Execution, error) {
	ex, err := scanExecution(rt.DB.Pool.QueryRow(ctx, `SELECT `+execCols+` FROM executions e
		JOIN workflow_versions v ON v.id = e.version_id WHERE e.id::text=$1 AND e.workspace_id=$2`, id, workspaceID))
	if persistence.IsInvalidText(err) {
		return nil, persistence.ErrNotFound
	}
	return ex, err
}

// WorkspaceOf returns the workspace that owns an execution.
func (rt *Runtime) WorkspaceOf(ctx context.Context, id string) (string, error) {
	var ws string
	err := rt.DB.Pool.QueryRow(ctx, `SELECT workspace_id::text FROM executions WHERE id::text=$1`, id).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) || persistence.IsInvalidText(err) {
		return "", persistence.ErrNotFound
	}
	return ws, err
}

// Nodes returns the per-node state of an execution with inputs and outputs.
func (rt *Runtime) Nodes(ctx context.Context, execID string) ([]NodeExec, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT node_id, node_type, state, branch, attempt, input, output, error, wake_at,
		coalesce(worker_id,''), started_at, finished_at FROM node_executions WHERE execution_id=$1 ORDER BY node_id`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeExec{}
	for rows.Next() {
		var n NodeExec
		var state string
		var in, o, e []byte
		if err := rows.Scan(&n.NodeID, &n.NodeType, &state, &n.Branch, &n.Attempt, &in, &o, &e, &n.WakeAt, &n.WorkerID, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, err
		}
		n.State = engine.NodeState(state)
		n.Input, n.Output = unmarshalAny(in), unmarshalAny(o)
		if len(e) > 0 && string(e) != "null" {
			var ne engine.NodeError
			if json.Unmarshal(e, &ne) == nil {
				n.Error = &ne
			}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Attempt is one delivery attempt of a node.
type Attempt struct {
	Attempt       int               `json:"attempt"`
	NodeID        string            `json:"node_id"`
	Status        string            `json:"status"`
	DeliveryCount int               `json:"delivery_count"`
	WorkerID      string            `json:"worker_id,omitempty"`
	Error         *engine.NodeError `json:"error,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	StartedAt     *time.Time        `json:"started_at,omitempty"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
}

// Attempts lists the task attempts of an execution.
func (rt *Runtime) Attempts(ctx context.Context, execID string) ([]Attempt, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT node_id, attempt, status, delivery_count, coalesce(leased_by,''), last_error,
		created_at, started_at, finished_at FROM tasks WHERE execution_id=$1 ORDER BY created_at, attempt`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var e []byte
		if err := rows.Scan(&a.NodeID, &a.Attempt, &a.Status, &a.DeliveryCount, &a.WorkerID, &e, &a.CreatedAt, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		if len(e) > 0 && string(e) != "null" {
			var ne engine.NodeError
			if json.Unmarshal(e, &ne) == nil {
				a.Error = &ne
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Events returns events after the given id, oldest first.
func (rt *Runtime) Events(ctx context.Context, execID string, after int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := rt.DB.Pool.Query(ctx, `SELECT id, execution_id::text, type, node_id, attempt, data, created_at
		FROM execution_events WHERE execution_id=$1 AND id > $2 ORDER BY id LIMIT $3`, execID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var d []byte
		if err := rows.Scan(&e.ID, &e.ExecutionID, &e.Type, &e.NodeID, &e.Attempt, &d, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(d) > 0 {
			_ = json.Unmarshal(d, &e.Data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListParams filters executions.
type ListParams struct {
	WorkspaceID string
	WorkflowID  string
	Status      string
	Limit       int
	AfterTime   *time.Time
	AfterID     string
	ReplayOf    string
}

// Summary is an execution without payloads.
type Summary struct {
	ID          string           `json:"id"`
	WorkflowID  string           `json:"workflow_id"`
	Version     int              `json:"version"`
	Status      engine.ExecState `json:"status"`
	TriggerType string           `json:"trigger_type"`
	Error       string           `json:"error,omitempty"`
	ReplayOf    *string          `json:"replay_of,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	StartedAt   *time.Time       `json:"started_at,omitempty"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
}

// List returns root executions newest first (keyset paginated).
func (rt *Runtime) List(ctx context.Context, p ListParams) ([]Summary, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	q := `SELECT e.id::text, e.workflow_id::text, v.version, e.status, e.trigger_type, coalesce(e.error,''), e.replay_of::text,
		e.created_at, e.started_at, e.finished_at FROM executions e JOIN workflow_versions v ON v.id = e.version_id
		WHERE e.workspace_id = $1 AND e.kind = 'root'`
	args := []any{p.WorkspaceID}
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if p.WorkflowID != "" {
		add("e.workflow_id::text = $%d", p.WorkflowID)
	}
	if p.Status != "" {
		add("e.status = $%d", p.Status)
	}
	if p.ReplayOf != "" {
		add("e.replay_of::text = $%d", p.ReplayOf)
	}
	if p.AfterTime != nil {
		args = append(args, *p.AfterTime, p.AfterID)
		q += fmt.Sprintf(" AND (e.created_at, e.id::text) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, p.Limit)
	q += fmt.Sprintf(" ORDER BY e.created_at DESC, e.id DESC LIMIT $%d", len(args))
	rows, err := rt.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		if persistence.IsInvalidText(err) {
			return []Summary{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		var st string
		if err := rows.Scan(&s.ID, &s.WorkflowID, &s.Version, &st, &s.TriggerType, &s.Error, &s.ReplayOf, &s.CreatedAt, &s.StartedAt, &s.FinishedAt); err != nil {
			return nil, err
		}
		s.Status = engine.ExecState(st)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Children lists the child executions of a node (foreach items or sub-workflow).
func (rt *Runtime) Children(ctx context.Context, execID string) ([]Summary, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT e.id::text, e.workflow_id::text, v.version, e.status, e.trigger_type, coalesce(e.error,''),
		e.replay_of::text, e.created_at, e.started_at, e.finished_at FROM executions e JOIN workflow_versions v ON v.id = e.version_id
		WHERE e.parent_execution_id=$1 ORDER BY e.parent_node_id, e.item_index`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		var st string
		if err := rows.Scan(&s.ID, &s.WorkflowID, &s.Version, &st, &s.TriggerType, &s.Error, &s.ReplayOf, &s.CreatedAt, &s.StartedAt, &s.FinishedAt); err != nil {
			return nil, err
		}
		s.Status = engine.ExecState(st)
		out = append(out, s)
	}
	return out, rows.Err()
}
