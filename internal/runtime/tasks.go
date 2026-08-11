package runtime

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
)

// NotifyChannel is the Postgres channel used to wake idle workers.
const NotifyChannel = "synapse_tasks"

// Task is a claimed unit of work.
type Task struct {
	ID             string
	ExecutionID    string
	NodeID         string
	NodeType       string
	Attempt        int
	LeaseToken     string
	IdempotencyKey string
	Timeout        time.Duration
	DeliveryCount  int
}

// Ref identifies a lease for completion and heartbeats.
func (t Task) Ref() TaskRef { return TaskRef{ID: t.ID, LeaseToken: t.LeaseToken} }

// TaskRef carries the fencing token for a lease.
type TaskRef struct{ ID, LeaseToken string }

// Work is everything a worker needs to run a claimed task.
type Work struct {
	Task        Task
	WorkspaceID string
	Node        *workflow.Node
	Ctx         *engine.Context
}

func (rt *Runtime) nodeTimeout(n *workflow.Node) time.Duration {
	if n.TimeoutMS > 0 {
		return time.Duration(n.TimeoutMS) * time.Millisecond
	}
	return rt.DefaultNodeTimeout
}

// enqueue creates the first attempt for a ready worker node.
func (r *run) enqueue(n *workflow.Node, attempt int) error {
	if err := r.set(n.ID, engine.NodeQueued); err != nil {
		return err
	}
	r.rows[n.ID].Attempt = attempt
	if err := r.insertTask(n, attempt, 0); err != nil {
		return err
	}
	r.emit(EvNodeQueued, n.ID, attempt, nil)
	return nil
}

// insertTask adds a task row that becomes claimable after delay.
func (r *run) insertTask(n *workflow.Node, attempt int, delay time.Duration) error {
	_, err := r.tx.Exec(r.ctx, `INSERT INTO tasks (execution_id, node_id, node_type, attempt, run_at, max_deliveries, timeout_ms, idempotency_key)
		VALUES ($1,$2,$3,$4, now() + $5 * interval '1 millisecond', $6, $7, $8)
		ON CONFLICT (execution_id, node_id, attempt) DO NOTHING`,
		r.ex.ID, n.ID, string(n.Type), attempt, delay.Milliseconds(), r.rt.MaxDeliveries,
		r.rt.nodeTimeout(n).Milliseconds(), fmt.Sprintf("%s:%s:%d", r.ex.ID, n.ID, attempt))
	if err != nil {
		return err
	}
	if delay <= 0 {
		_, err = r.tx.Exec(r.ctx, `SELECT pg_notify($1, '')`, NotifyChannel)
	}
	return err
}

// Claim leases up to limit ready tasks for the worker. Tasks of excluded node
// types are left in the queue (per-type concurrency limits).
func (rt *Runtime) Claim(ctx context.Context, workerID string, limit int, exclude []string) ([]Task, error) {
	if limit <= 0 {
		return nil, nil
	}
	if exclude == nil {
		exclude = []string{}
	}
	rows, err := rt.DB.Pool.Query(ctx, `
		UPDATE tasks t SET status='leased', lease_token=gen_random_uuid(), leased_by=$1,
			lease_expires_at = now() + $2 * interval '1 millisecond', delivery_count = t.delivery_count + 1, started_at = now()
		WHERE t.id IN (
			SELECT id FROM tasks WHERE status='queued' AND run_at <= now() AND NOT (node_type = ANY($4))
			ORDER BY priority DESC, run_at, created_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING t.id::text, t.execution_id::text, t.node_id, t.node_type, t.attempt, t.lease_token::text,
			t.idempotency_key, t.timeout_ms, t.delivery_count`,
		workerID, rt.LeaseDuration.Milliseconds(), limit, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var ms int
		if err := rows.Scan(&t.ID, &t.ExecutionID, &t.NodeID, &t.NodeType, &t.Attempt, &t.LeaseToken, &t.IdempotencyKey, &ms, &t.DeliveryCount); err != nil {
			return nil, err
		}
		t.Timeout = time.Duration(ms) * time.Millisecond
		out = append(out, t)
	}
	return out, rows.Err()
}

// taskRow is the locked task state seen inside a run.
type taskRow struct {
	ID      string
	Node    string
	Attempt int
	Status  string
	Token   *string
}

// takeTask verifies the caller still owns the lease and closes the task with
// the final status. It returns nil (not an error) when the result must be
// dropped; the caller then just returns.
func (r *run) takeTask(ref TaskRef, final string, lastErr *engine.NodeError) (*taskRow, error) {
	var t taskRow
	err := r.tx.QueryRow(r.ctx, `SELECT id::text, node_id, attempt, status, lease_token::text FROM tasks
		WHERE id=$1 AND execution_id=$2 FOR UPDATE`, ref.ID, r.ex.ID).Scan(&t.ID, &t.Node, &t.Attempt, &t.Status, &t.Token)
	if err != nil {
		return nil, persistence.NotFound(err)
	}
	if t.Status != taskLeased || t.Token == nil || *t.Token != ref.LeaseToken {
		return nil, nil
	}
	if r.ex.Status.Terminal() || r.ex.Status == engine.ExecCancelling {
		if _, err := r.tx.Exec(r.ctx, `UPDATE tasks SET status='cancelled', finished_at=now() WHERE id=$1`, t.ID); err != nil {
			return nil, err
		}
		if r.ex.Status == engine.ExecCancelling {
			if s := r.st[t.Node]; s != nil && !s.State.Terminal() {
				r.force(t.Node, engine.NodeCancelled)
				r.emit(EvNodeCancelled, t.Node, t.Attempt, nil)
			}
		}
		return nil, nil
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE tasks SET status=$2, finished_at=now(), last_error=$3 WHERE id=$1`,
		t.ID, final, nullableJSON(lastErr)); err != nil {
		return nil, err
	}
	return &t, nil
}

func nullableJSON(e *engine.NodeError) any {
	if e == nil {
		return nil
	}
	return marshalJSON(e)
}

// Begin records that the worker started the task and returns its inputs. A
// nil Work with nil error is never returned; ErrLeaseLost means skip the task.
func (rt *Runtime) Begin(ctx context.Context, workerID string, t Task) (*Work, error) {
	var work *Work
	lost := false
	err := rt.mutate(ctx, t.ExecutionID, false, func(r *run) error {
		var status string
		var token *string
		if err := r.tx.QueryRow(ctx, `SELECT status, lease_token::text FROM tasks WHERE id=$1 FOR UPDATE`, t.ID).Scan(&status, &token); err != nil {
			return persistence.NotFound(err)
		}
		if status != taskLeased || token == nil || *token != t.LeaseToken {
			lost = true
			return nil
		}
		cancelTask := func() error {
			_, err := r.tx.Exec(ctx, `UPDATE tasks SET status='cancelled', finished_at=now() WHERE id=$1`, t.ID)
			lost = true
			return err
		}
		st := r.st[t.NodeID]
		if st == nil || r.ex.Status.Terminal() {
			return cancelTask()
		}
		if r.ex.Status == engine.ExecCancelling {
			if err := cancelTask(); err != nil {
				return err
			}
			if !st.State.Terminal() {
				r.force(t.NodeID, engine.NodeCancelled)
				r.emit(EvNodeCancelled, t.NodeID, t.Attempt, nil)
			}
			return r.settleCancelling()
		}
		switch st.State {
		case engine.NodeQueued, engine.NodeRetrying, engine.NodeRunning:
		default:
			return cancelTask()
		}
		if err := r.set(t.NodeID, engine.NodeRunning); err != nil {
			return err
		}
		row := r.rows[t.NodeID]
		row.Attempt = t.Attempt
		row.WorkerID = workerID
		now := time.Now()
		row.StartedAt = &now
		row.FinishedAt = nil
		r.dirty[t.NodeID] = true
		r.emit(EvNodeStarted, t.NodeID, t.Attempt, map[string]any{"worker": workerID, "delivery": t.DeliveryCount})
		n := r.gi.ix.Nodes[t.NodeID]
		ectx, err := r.engineCtx(r.gi.refs[t.NodeID])
		if err != nil {
			return err
		}
		work = &Work{Task: t, WorkspaceID: r.ex.WorkspaceID, Node: n, Ctx: ectx}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if lost || work == nil {
		return nil, ErrLeaseLost
	}
	return work, nil
}

func (rt *Runtime) taskExecution(ctx context.Context, taskID string) (string, error) {
	var id string
	err := rt.DB.Pool.QueryRow(ctx, `SELECT execution_id::text FROM tasks WHERE id=$1`, taskID).Scan(&id)
	return id, persistence.NotFound(err)
}

// Complete records a successful attempt and advances the execution.
func (rt *Runtime) Complete(ctx context.Context, ref TaskRef, output any) error {
	execID, err := rt.taskExecution(ctx, ref.ID)
	if err != nil {
		return err
	}
	lost := false
	err = rt.mutate(ctx, execID, true, func(r *run) error {
		t, err := r.takeTask(ref, taskDone, nil)
		if err != nil {
			return err
		}
		if t == nil {
			lost = true
			return nil
		}
		return r.succeed(r.gi.ix.Nodes[t.Node], output, "")
	})
	if err != nil {
		return err
	}
	if lost {
		return ErrLeaseLost
	}
	return nil
}

// Fail records a failed attempt: it schedules a retry when the error is
// retryable and attempts remain, otherwise it fails the node.
func (rt *Runtime) Fail(ctx context.Context, ref TaskRef, ne *engine.NodeError) error {
	execID, err := rt.taskExecution(ctx, ref.ID)
	if err != nil {
		return err
	}
	lost := false
	err = rt.mutate(ctx, execID, true, func(r *run) error {
		t, err := r.takeTask(ref, taskFailed, ne)
		if err != nil {
			return err
		}
		if t == nil {
			lost = true
			return nil
		}
		return r.attemptFailed(r.gi.ix.Nodes[t.Node], t.Attempt, ne)
	})
	if err != nil {
		return err
	}
	if lost {
		return ErrLeaseLost
	}
	return nil
}

// attemptFailed applies the retry policy after attempt failed.
func (r *run) attemptFailed(n *workflow.Node, attempt int, ne *engine.NodeError) error {
	rnd := r.rt.Rnd
	if rnd == nil {
		rnd = rand.Float64
	}
	row := r.rows[n.ID]
	if ne.Retryable && n.Retry.CanRetry(attempt) {
		delay := n.Retry.Delay(attempt, rnd)
		if err := r.set(n.ID, engine.NodeRetrying); err != nil {
			return err
		}
		row.Error = ne
		row.WorkerID = ""
		r.dirty[n.ID] = true
		if err := r.insertTask(n, attempt+1, delay); err != nil {
			return err
		}
		r.emit(EvNodeRetrying, n.ID, attempt, map[string]any{
			"error": ne, "next_attempt": attempt + 1, "delay_ms": delay.Milliseconds(),
		})
		return nil
	}
	return r.failNode(n, ne)
}

// Release returns a task to the queue without counting the delivery; used by
// workers that are shutting down.
func (rt *Runtime) Release(ctx context.Context, ref TaskRef) error {
	tag, err := rt.DB.Pool.Exec(ctx, `UPDATE tasks SET status='queued', lease_token=NULL, leased_by=NULL, lease_expires_at=NULL,
		delivery_count = GREATEST(delivery_count-1, 0), run_at = now()
		WHERE id=$1 AND lease_token=$2 AND status='leased'`, ref.ID, ref.LeaseToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	_, err = rt.DB.Pool.Exec(ctx, `SELECT pg_notify($1, '')`, NotifyChannel)
	return err
}

// HeartbeatResult reports leases the worker must stop working on.
type HeartbeatResult struct {
	Lost      []string // task ids reassigned or already finished
	Cancelled []string // task ids whose execution was cancelled or ended
}

// Heartbeat extends the leases and refreshes the worker's liveness.
func (rt *Runtime) Heartbeat(ctx context.Context, workerID string, refs []TaskRef) (HeartbeatResult, error) {
	var res HeartbeatResult
	if _, err := rt.DB.Pool.Exec(ctx, `UPDATE workers SET last_seen_at=now() WHERE id=$1 AND status <> 'dead'`, workerID); err != nil {
		return res, err
	}
	if len(refs) == 0 {
		return res, nil
	}
	ids, toks := make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		ids[i], toks[i] = r.ID, r.LeaseToken
	}
	rows, err := rt.DB.Pool.Query(ctx, `UPDATE tasks t SET lease_expires_at = now() + $3 * interval '1 millisecond'
		FROM unnest($1::uuid[], $2::uuid[]) AS x(id, tok)
		WHERE t.id = x.id AND t.lease_token = x.tok AND t.status='leased' RETURNING t.id::text`, ids, toks, rt.LeaseDuration.Milliseconds())
	if err != nil {
		return res, err
	}
	alive := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		alive[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, r := range refs {
		if !alive[r.ID] {
			res.Lost = append(res.Lost, r.ID)
		}
	}
	if len(alive) == 0 {
		return res, nil
	}
	aliveIDs := make([]string, 0, len(alive))
	for id := range alive {
		aliveIDs = append(aliveIDs, id)
	}
	crows, err := rt.DB.Pool.Query(ctx, `SELECT t.id::text FROM tasks t JOIN executions e ON e.id = t.execution_id
		WHERE t.id = ANY($1::uuid[]) AND e.status IN ('cancelling','cancelled','failed','succeeded')`, aliveIDs)
	if err != nil {
		return res, err
	}
	defer crows.Close()
	for crows.Next() {
		var id string
		if err := crows.Scan(&id); err != nil {
			return res, err
		}
		res.Cancelled = append(res.Cancelled, id)
	}
	return res, crows.Err()
}

// RegisterWorker upserts the worker row.
func (rt *Runtime) RegisterWorker(ctx context.Context, id, host string, capacity int) error {
	_, err := rt.DB.Pool.Exec(ctx, `INSERT INTO workers (id, hostname, capacity, status) VALUES ($1,$2,$3,'active')
		ON CONFLICT (id) DO UPDATE SET hostname=$2, capacity=$3, status='active', started_at=now(), last_seen_at=now()`, id, host, capacity)
	return err
}

// SetWorkerStatus marks the worker draining or stopped.
func (rt *Runtime) SetWorkerStatus(ctx context.Context, id, status string) error {
	_, err := rt.DB.Pool.Exec(ctx, `UPDATE workers SET status=$2, last_seen_at=now() WHERE id=$1`, id, status)
	return err
}

// Reaper operations ---------------------------------------------------------

// ReapLeases requeues tasks whose lease expired or whose worker is dead and
// fails tasks that exhausted their deliveries. It only touches task rows
// (never the execution lock) except when failing an exhausted task.
func (rt *Runtime) ReapLeases(ctx context.Context, deadWorkers []string) (requeued int, err error) {
	if deadWorkers == nil {
		deadWorkers = []string{}
	}
	tag, err := rt.DB.Pool.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM tasks WHERE status='leased' AND (lease_expires_at < now() OR leased_by = ANY($1))
			ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED)
		UPDATE tasks t SET status = CASE WHEN t.delivery_count >= t.max_deliveries THEN 'dead' ELSE 'queued' END,
			run_at = now(), lease_token = NULL, leased_by = NULL, lease_expires_at = NULL
		FROM expired WHERE t.id = expired.id`, deadWorkers)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		_, _ = rt.DB.Pool.Exec(ctx, `SELECT pg_notify($1, '')`, NotifyChannel)
	}
	_, err = rt.failDeadTasks(ctx)
	return int(tag.RowsAffected()), err
}

// failDeadTasks fails nodes whose task ran out of deliveries.
func (rt *Runtime) failDeadTasks(ctx context.Context) (int, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT t.id::text, t.execution_id::text FROM tasks t
		JOIN node_executions n ON n.execution_id = t.execution_id AND n.node_id = t.node_id
		WHERE t.status='dead' AND n.state IN ('queued','retrying','running') AND n.attempt = t.attempt LIMIT 200`)
	if err != nil {
		return 0, err
	}
	type dt struct{ id, exec string }
	var list []dt
	for rows.Next() {
		var d dt
		if err := rows.Scan(&d.id, &d.exec); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		err := rt.mutate(ctx, d.exec, true, func(r *run) error {
			var attempt int
			var node, status string
			if err := r.tx.QueryRow(ctx, `SELECT node_id, attempt, status FROM tasks WHERE id=$1 FOR UPDATE`, d.id).Scan(&node, &attempt, &status); err != nil {
				return persistence.NotFound(err)
			}
			st := r.st[node]
			if status != taskDead || st == nil || st.State.Terminal() || r.ex.Status.Terminal() {
				return nil
			}
			n++
			return r.failNode(r.gi.ix.Nodes[node], &engine.NodeError{
				Code: engine.CodeLeaseLost, Message: "the task was delivered too many times without completing",
			})
		})
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

// DeadWorkers marks silent workers dead and returns their ids.
func (rt *Runtime) DeadWorkers(ctx context.Context, after time.Duration) ([]string, error) {
	rows, err := rt.DB.Pool.Query(ctx, `UPDATE workers SET status='dead'
		WHERE status IN ('active','draining') AND last_seen_at < now() - $1 * interval '1 millisecond' RETURNING id`, after.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TimeoutTasks fails leased tasks that outlived their timeout plus a grace
// period; it catches workers that hang while still heartbeating.
func (rt *Runtime) TimeoutTasks(ctx context.Context, grace time.Duration) (int, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT id::text, execution_id::text FROM tasks
		WHERE status='leased' AND timeout_ms > 0 AND started_at + (timeout_ms + $1) * interval '1 millisecond' < now() LIMIT 200`, grace.Milliseconds())
	if err != nil {
		return 0, err
	}
	type tt struct{ id, exec string }
	var list []tt
	for rows.Next() {
		var t tt
		if err := rows.Scan(&t.id, &t.exec); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		err := rt.mutate(ctx, t.exec, true, func(r *run) error {
			var node, status string
			var attempt int
			var expired bool
			if err := r.tx.QueryRow(ctx, `SELECT node_id, attempt, status,
				(started_at + (timeout_ms + $2) * interval '1 millisecond' < now()) FROM tasks WHERE id=$1 FOR UPDATE`,
				t.id, grace.Milliseconds()).Scan(&node, &attempt, &status, &expired); err != nil {
				return persistence.NotFound(err)
			}
			if status != taskLeased || !expired {
				return nil
			}
			ne := &engine.NodeError{Code: engine.CodeTimeout, Message: "the node exceeded its timeout", Retryable: true}
			// A fresh state for the lease makes the stale worker's result fenced.
			if _, err := r.tx.Exec(ctx, `UPDATE tasks SET status='failed', finished_at=now(), last_error=$2 WHERE id=$1`, t.id, marshalJSON(ne)); err != nil {
				return err
			}
			if r.ex.Status.Terminal() || r.ex.Status == engine.ExecCancelling {
				return nil
			}
			n++
			return r.attemptFailed(r.gi.ix.Nodes[node], attempt, ne)
		})
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

// WakeDue completes delay nodes whose wake time has passed.
func (rt *Runtime) WakeDue(ctx context.Context, limit int) (int, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT execution_id::text, node_id FROM node_executions
		WHERE state='waiting' AND node_type='delay' AND wake_at <= now() ORDER BY wake_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type key struct{ exec, node string }
	var due []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.exec, &k.node); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, k := range due {
		err := rt.mutate(ctx, k.exec, true, func(r *run) error {
			st := r.st[k.node]
			if st == nil || st.State != engine.NodeWaiting || r.ex.Status.Terminal() || r.ex.Status == engine.ExecCancelling {
				return nil
			}
			outs, err := r.outputs([]string{k.node})
			if err != nil {
				return err
			}
			n++
			return r.succeed(r.gi.ix.Nodes[k.node], outs[k.node], "")
		})
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

// ExpireDeadlines fails executions past their workflow timeout.
func (rt *Runtime) ExpireDeadlines(ctx context.Context, limit int) (int, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT id::text FROM executions
		WHERE deadline_at < now() AND status IN ('running','waiting') ORDER BY deadline_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := rt.mutate(ctx, id, false, func(r *run) error {
			if r.ex.Status.Terminal() || r.ex.Status == engine.ExecCancelling || r.ex.DeadlineAt == nil {
				return nil
			}
			n++
			return r.terminate(engine.ExecFailed, "workflow exceeded its timeout")
		})
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

// SweepChildren re-advances parents whose finished children were never
// acknowledged (the notifying process crashed between commit and callback).
func (rt *Runtime) SweepChildren(ctx context.Context, after time.Duration, limit int) (int, error) {
	rows, err := rt.DB.Pool.Query(ctx, `SELECT DISTINCT parent_execution_id::text FROM executions
		WHERE parent_execution_id IS NOT NULL AND finished_at IS NOT NULL AND notified_at IS NULL
		AND finished_at < now() - $1 * interval '1 millisecond' LIMIT $2`, after.Milliseconds(), limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := rt.AdvanceParent(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// QueueDepth counts claimable and delayed queued tasks, capped at max.
func (rt *Runtime) QueueDepth(ctx context.Context, max int) (int, error) {
	var n int
	err := rt.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM tasks WHERE status='queued' LIMIT $1) q`, max).Scan(&n)
	return n, err
}
