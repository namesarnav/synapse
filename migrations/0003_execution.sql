-- Durable execution state: executions, per-node state, the task queue, the
-- event log and the worker registry. PostgreSQL is the only source of truth.

CREATE TABLE executions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    workflow_id         UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version_id          UUID NOT NULL REFERENCES workflow_versions(id),
    kind                TEXT NOT NULL DEFAULT 'root' CHECK (kind IN ('root','foreach_item','sub_workflow')),
    status              TEXT NOT NULL DEFAULT 'created'
                        CHECK (status IN ('created','running','waiting','succeeded','failed','cancelling','cancelled')),
    trigger_type        TEXT NOT NULL DEFAULT 'manual',
    trigger_payload     JSONB NOT NULL DEFAULT 'null',
    start_node          TEXT NOT NULL DEFAULT '',
    context             JSONB NOT NULL DEFAULT '{}',
    output              JSONB,
    error               TEXT,
    idempotency_key     TEXT,
    parent_execution_id UUID REFERENCES executions(id) ON DELETE CASCADE,
    parent_node_id      TEXT,
    item_index          INTEGER,
    depth               INTEGER NOT NULL DEFAULT 0,
    replay_of           UUID REFERENCES executions(id) ON DELETE SET NULL,
    replay_source_node  TEXT,
    created_by          UUID REFERENCES users(id) ON DELETE SET NULL,
    deadline_at         TIMESTAMPTZ,
    notified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX executions_workflow_idx ON executions (workflow_id, created_at DESC, id) WHERE kind = 'root';
CREATE INDEX executions_workspace_idx ON executions (workspace_id, created_at DESC, id) WHERE kind = 'root';
CREATE INDEX executions_status_idx ON executions (status) WHERE status IN ('created','running','waiting','cancelling');
CREATE INDEX executions_deadline_idx ON executions (deadline_at) WHERE deadline_at IS NOT NULL AND status IN ('running','waiting');
CREATE INDEX executions_parent_idx ON executions (parent_execution_id, parent_node_id);
CREATE UNIQUE INDEX executions_child_unique ON executions (parent_execution_id, parent_node_id, item_index)
    WHERE parent_execution_id IS NOT NULL;
CREATE UNIQUE INDEX executions_idem_unique ON executions (workflow_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND kind = 'root';
CREATE INDEX executions_unnotified_idx ON executions (finished_at)
    WHERE parent_execution_id IS NOT NULL AND finished_at IS NOT NULL AND notified_at IS NULL;
CREATE INDEX executions_replay_idx ON executions (replay_of) WHERE replay_of IS NOT NULL;

CREATE TABLE node_executions (
    execution_id UUID NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    node_type    TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending','ready','queued','running','waiting','succeeded','failed','retrying','skipped','cancelled')),
    branch       TEXT NOT NULL DEFAULT '',
    on_error     TEXT NOT NULL DEFAULT 'fail',
    attempt      INTEGER NOT NULL DEFAULT 0,
    input        JSONB,
    output       JSONB,
    error        JSONB,
    wake_at      TIMESTAMPTZ,
    worker_id    TEXT,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (execution_id, node_id)
);
CREATE INDEX node_executions_wake_idx ON node_executions (wake_at) WHERE state = 'waiting' AND wake_at IS NOT NULL;

-- One row per delivery unit: (execution, node, attempt). Redelivery after a
-- lease expiry keeps the attempt; an explicit failure creates the next one.
CREATE TABLE tasks (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_id     UUID NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    node_id          TEXT NOT NULL,
    node_type        TEXT NOT NULL,
    attempt          INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','leased','succeeded','failed','cancelled','dead')),
    run_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    priority         INTEGER NOT NULL DEFAULT 0,
    lease_token      UUID,
    leased_by        TEXT,
    lease_expires_at TIMESTAMPTZ,
    delivery_count   INTEGER NOT NULL DEFAULT 0,
    max_deliveries   INTEGER NOT NULL DEFAULT 5,
    timeout_ms       INTEGER NOT NULL DEFAULT 0,
    idempotency_key  TEXT NOT NULL,
    last_error       JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ,
    UNIQUE (execution_id, node_id, attempt)
);
CREATE INDEX tasks_claim_idx ON tasks (priority DESC, run_at, created_at) WHERE status = 'queued';
CREATE INDEX tasks_lease_idx ON tasks (lease_expires_at) WHERE status = 'leased';
CREATE INDEX tasks_execution_idx ON tasks (execution_id);
CREATE INDEX tasks_worker_idx ON tasks (leased_by) WHERE status = 'leased';

CREATE TABLE execution_events (
    id           BIGSERIAL PRIMARY KEY,
    execution_id UUID NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    type         TEXT NOT NULL,
    node_id      TEXT NOT NULL DEFAULT '',
    attempt      INTEGER NOT NULL DEFAULT 0,
    data         JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX execution_events_exec_idx ON execution_events (execution_id, id);

CREATE TABLE workers (
    id           TEXT PRIMARY KEY,
    hostname     TEXT NOT NULL DEFAULT '',
    capacity     INTEGER NOT NULL DEFAULT 1,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','draining','stopped','dead'))
);
CREATE INDEX workers_seen_idx ON workers (last_seen_at);
