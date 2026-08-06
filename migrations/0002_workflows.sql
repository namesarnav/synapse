-- Workflows: mutable draft plus immutable published versions.

CREATE TABLE workflows (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id         UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active')),
    draft_graph          JSONB NOT NULL DEFAULT '{"nodes":[],"edges":[]}',
    revision             INTEGER NOT NULL DEFAULT 1,
    published_version_id UUID,
    created_by           UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at           TIMESTAMPTZ
);
CREATE INDEX workflows_workspace_idx ON workflows (workspace_id, created_at DESC, id) WHERE deleted_at IS NULL;

CREATE TABLE workflow_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    graph       JSONB NOT NULL,
    graph_hash  TEXT NOT NULL,
    notes       TEXT NOT NULL DEFAULT '',
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workflow_id, version)
);

ALTER TABLE workflows
    ADD CONSTRAINT workflows_published_fk FOREIGN KEY (published_version_id) REFERENCES workflow_versions(id);

-- Published versions are immutable: executions and replays depend on it.
CREATE FUNCTION workflow_versions_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'workflow_versions rows are immutable' USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workflow_versions_no_update
    BEFORE UPDATE ON workflow_versions
    FOR EACH ROW EXECUTE FUNCTION workflow_versions_immutable();
