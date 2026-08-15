-- Triggers (schedules, webhook endpoints) and encrypted workspace secrets.

CREATE TABLE schedules (
    workflow_id  UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version_id   UUID NOT NULL REFERENCES workflow_versions(id) ON DELETE CASCADE,
    cron         TEXT NOT NULL,
    timezone     TEXT NOT NULL DEFAULT 'UTC',
    payload      JSONB NOT NULL DEFAULT '{}',
    next_run_at  TIMESTAMPTZ NOT NULL,
    last_run_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, node_id)
);
CREATE INDEX schedules_due_idx ON schedules (next_run_at);

CREATE TABLE webhook_endpoints (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    workflow_id  UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    hmac_secret  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workflow_id, node_id)
);

CREATE TABLE secrets (
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_]{0,63}$'),
    ciphertext   BYTEA NOT NULL,
    nonce        BYTEA NOT NULL,
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, name)
);
