// Package wfstore persists workflows and their immutable versions.
package wfstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
)

var (
	ErrStaleRevision = errors.New("workflow was modified by someone else")
	ErrNotPublished  = errors.New("workflow is not published")
)

type Workflow struct {
	ID                 string         `json:"id"`
	WorkspaceID        string         `json:"workspace_id"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	Status             string         `json:"status"`
	Graph              workflow.Graph `json:"graph"`
	Revision           int            `json:"revision"`
	PublishedVersionID *string        `json:"published_version_id"`
	PublishedVersion   *int           `json:"published_version"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// Summary omits the graph for list views.
type Summary struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	PublishedVersion *int      `json:"published_version"`
	NodeCount        int       `json:"node_count"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Version struct {
	ID         string         `json:"id"`
	WorkflowID string         `json:"workflow_id"`
	Version    int            `json:"version"`
	Graph      workflow.Graph `json:"graph,omitempty"`
	GraphHash  string         `json:"graph_hash"`
	Notes      string         `json:"notes"`
	CreatedAt  time.Time      `json:"created_at"`
}

type Store struct{ DB *persistence.DB }

const wfCols = `w.id::text, w.workspace_id::text, w.name, w.description, w.status, w.draft_graph, w.revision,
	w.published_version_id::text, v.version, w.created_at, w.updated_at`

func scanWorkflow(row pgx.Row) (Workflow, error) {
	var w Workflow
	var graph []byte
	err := row.Scan(&w.ID, &w.WorkspaceID, &w.Name, &w.Description, &w.Status, &graph, &w.Revision,
		&w.PublishedVersionID, &w.PublishedVersion, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return w, persistence.NotFound(err)
	}
	if err := json.Unmarshal(graph, &w.Graph); err != nil {
		return w, err
	}
	w.Graph.Normalize()
	return w, nil
}

func (s *Store) get(ctx context.Context, q persistence.Querier, ws, id string, lock bool) (Workflow, error) {
	sql := `SELECT ` + wfCols + ` FROM workflows w LEFT JOIN workflow_versions v ON v.id = w.published_version_id
	         WHERE w.id=$1 AND w.workspace_id=$2 AND w.deleted_at IS NULL`
	if lock {
		sql += ` FOR UPDATE OF w`
	}
	return scanWorkflow(q.QueryRow(ctx, sql, id, ws))
}

// Get returns a workflow in a workspace; other workspaces' ids look missing.
func (s *Store) Get(ctx context.Context, ws, id string) (Workflow, error) {
	return s.get(ctx, s.DB.Pool, ws, id, false)
}

func (s *Store) Create(ctx context.Context, ws, userID, name, desc string, g workflow.Graph) (Workflow, error) {
	g.Normalize()
	raw, _ := json.Marshal(g)
	var id string
	if err := s.DB.Pool.QueryRow(ctx,
		`INSERT INTO workflows (workspace_id, name, description, draft_graph, created_by) VALUES ($1,$2,$3,$4,$5) RETURNING id::text`,
		ws, name, desc, raw, userID).Scan(&id); err != nil {
		return Workflow{}, err
	}
	return s.Get(ctx, ws, id)
}

// List returns workflow summaries, newest first, with keyset pagination on
// (created_at, id) encoded as a cursor by the caller.
func (s *Store) List(ctx context.Context, ws, query string, limit int, afterCreated *time.Time, afterID string) ([]Summary, []time.Time, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []any{ws, "%" + query + "%", limit}
	sql := `SELECT w.id::text, w.name, w.description, w.status, v.version,
	               jsonb_array_length(w.draft_graph->'nodes'), w.updated_at, w.created_at
	          FROM workflows w LEFT JOIN workflow_versions v ON v.id = w.published_version_id
	         WHERE w.workspace_id=$1 AND w.deleted_at IS NULL AND w.name ILIKE $2`
	if afterCreated != nil {
		sql += ` AND (w.created_at, w.id) < ($4, $5::uuid)`
		args = append(args, *afterCreated, afterID)
	}
	sql += ` ORDER BY w.created_at DESC, w.id DESC LIMIT $3`
	rows, err := s.DB.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Summary{}
	var created []time.Time
	for rows.Next() {
		var sm Summary
		var c time.Time
		if err := rows.Scan(&sm.ID, &sm.Name, &sm.Description, &sm.Status, &sm.PublishedVersion, &sm.NodeCount, &sm.UpdatedAt, &c); err != nil {
			return nil, nil, err
		}
		out = append(out, sm)
		created = append(created, c)
	}
	return out, created, rows.Err()
}

// Update applies changes under optimistic concurrency: the caller supplies the
// revision it read, and a mismatch returns ErrStaleRevision.
type Update struct {
	Name        *string
	Description *string
	Graph       *workflow.Graph
	Revision    int
}

func (s *Store) Update(ctx context.Context, ws, id string, u Update) (Workflow, error) {
	var out Workflow
	err := s.DB.InTx(ctx, func(tx *persistence.Tx) error {
		cur, err := s.get(ctx, tx, ws, id, true)
		if err != nil {
			return err
		}
		if u.Revision != 0 && u.Revision != cur.Revision {
			return ErrStaleRevision
		}
		if u.Name != nil {
			cur.Name = *u.Name
		}
		if u.Description != nil {
			cur.Description = *u.Description
		}
		if u.Graph != nil {
			u.Graph.Normalize()
			cur.Graph = *u.Graph
		}
		raw, _ := json.Marshal(cur.Graph)
		if _, err := tx.Exec(ctx,
			`UPDATE workflows SET name=$1, description=$2, draft_graph=$3, revision=revision+1, updated_at=now() WHERE id=$4`,
			cur.Name, cur.Description, raw, id); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, ws, id, false)
		return err
	})
	return out, err
}

// Delete soft-deletes a workflow and unpublishes it.
func (s *Store) Delete(ctx context.Context, ws, id string) error {
	tag, err := s.DB.Pool.Exec(ctx,
		`UPDATE workflows SET deleted_at=now(), status='draft', published_version_id=NULL, updated_at=now()
		  WHERE id=$1 AND workspace_id=$2 AND deleted_at IS NULL`, id, ws)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// GraphHash returns a stable content hash of a normalized graph.
func GraphHash(g workflow.Graph) string {
	g.Normalize()
	raw, _ := json.Marshal(g)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Publish snapshots the draft into a new immutable version and activates it.
// Publishing an unchanged draft returns the current version (idempotent).
// validate is run against the locked draft; a non-nil error aborts.
func (s *Store) Publish(ctx context.Context, ws, id, userID, notes string, validate func(workflow.Graph) error) (Version, bool, error) {
	var v Version
	created := false
	err := s.DB.InTx(ctx, func(tx *persistence.Tx) error {
		created = false
		cur, err := s.get(ctx, tx, ws, id, true)
		if err != nil {
			return err
		}
		if err := validate(cur.Graph); err != nil {
			return err
		}
		hash := GraphHash(cur.Graph)
		if cur.PublishedVersionID != nil {
			existing, err := s.getVersionByID(ctx, tx, *cur.PublishedVersionID)
			if err != nil {
				return err
			}
			if existing.GraphHash == hash {
				v = existing
				return nil
			}
		}
		raw, _ := json.Marshal(cur.Graph)
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM workflow_versions WHERE workflow_id=$1`, id).Scan(&next); err != nil {
			return err
		}
		var vid string
		if err := tx.QueryRow(ctx,
			`INSERT INTO workflow_versions (workflow_id, version, graph, graph_hash, notes, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`, id, next, raw, hash, notes, userID).Scan(&vid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE workflows SET published_version_id=$1, status='active', updated_at=now() WHERE id=$2`, vid, id); err != nil {
			return err
		}
		v, err = s.getVersionByID(ctx, tx, vid)
		created = true
		return err
	})
	return v, created, err
}

// Unpublish deactivates a workflow; existing executions keep their pinned version.
func (s *Store) Unpublish(ctx context.Context, ws, id string) error {
	tag, err := s.DB.Pool.Exec(ctx,
		`UPDATE workflows SET published_version_id=NULL, status='draft', updated_at=now()
		  WHERE id=$1 AND workspace_id=$2 AND deleted_at IS NULL`, id, ws)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func (s *Store) getVersionByID(ctx context.Context, q persistence.Querier, id string) (Version, error) {
	return scanVersion(q.QueryRow(ctx,
		`SELECT id::text, workflow_id::text, version, graph, graph_hash, notes, created_at FROM workflow_versions WHERE id=$1`, id))
}

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var graph []byte
	if err := row.Scan(&v.ID, &v.WorkflowID, &v.Version, &graph, &v.GraphHash, &v.Notes, &v.CreatedAt); err != nil {
		return v, persistence.NotFound(err)
	}
	if err := json.Unmarshal(graph, &v.Graph); err != nil {
		return v, err
	}
	return v, nil
}

// VersionByID loads any version by id (used by the runtime, which pins one per execution).
func (s *Store) VersionByID(ctx context.Context, q persistence.Querier, id string) (Version, error) {
	return s.getVersionByID(ctx, q, id)
}

// GetVersion loads a numbered version of a workflow in a workspace.
func (s *Store) GetVersion(ctx context.Context, ws, workflowID string, n int) (Version, error) {
	return scanVersion(s.DB.Pool.QueryRow(ctx,
		`SELECT v.id::text, v.workflow_id::text, v.version, v.graph, v.graph_hash, v.notes, v.created_at
		   FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id
		  WHERE v.workflow_id=$1 AND v.version=$2 AND w.workspace_id=$3 AND w.deleted_at IS NULL`, workflowID, n, ws))
}

// Versions lists versions (without graphs) newest first.
func (s *Store) Versions(ctx context.Context, ws, workflowID string) ([]Version, error) {
	rows, err := s.DB.Pool.Query(ctx,
		`SELECT v.id::text, v.workflow_id::text, v.version, v.graph_hash, v.notes, v.created_at
		   FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id
		  WHERE v.workflow_id=$1 AND w.workspace_id=$2 AND w.deleted_at IS NULL ORDER BY v.version DESC`, workflowID, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.WorkflowID, &v.Version, &v.GraphHash, &v.Notes, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Published returns the active version of a workflow, or ErrNotPublished.
func (s *Store) Published(ctx context.Context, ws, workflowID string) (Version, error) {
	w, err := s.Get(ctx, ws, workflowID)
	if err != nil {
		return Version{}, err
	}
	if w.PublishedVersionID == nil {
		return Version{}, ErrNotPublished
	}
	return s.getVersionByID(ctx, s.DB.Pool, *w.PublishedVersionID)
}
