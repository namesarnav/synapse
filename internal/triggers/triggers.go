// Package triggers turns published workflows into live triggers: cron
// schedules that the scheduler fires and webhook endpoints with stable URLs.
package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/cron"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
)

var ErrNoEndpoint = errors.New("webhook endpoint not found")

// Store manages trigger rows and fires them.
type Store struct {
	DB  *persistence.DB
	RT  *runtime.Runtime
	Log *slog.Logger
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Sync aligns schedule and webhook rows with a workflow's published version.
// It is idempotent and safe to call after every publish, unpublish or delete.
func (s *Store) Sync(ctx context.Context, workspaceID, workflowID string) error {
	return s.DB.InTx(ctx, func(tx *persistence.Tx) error {
		var versionID string
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT v.id::text, v.graph FROM workflows w
			JOIN workflow_versions v ON v.id = w.published_version_id
			WHERE w.id=$1 AND w.workspace_id=$2 AND w.deleted_at IS NULL`, workflowID, workspaceID).Scan(&versionID, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			// Unpublished or deleted: stop firing. Webhook rows stay so URLs survive republish.
			_, err = tx.Exec(ctx, `DELETE FROM schedules WHERE workflow_id=$1`, workflowID)
			return err
		}
		if err != nil {
			return err
		}
		var g workflow.Graph
		if err := json.Unmarshal(raw, &g); err != nil {
			return fmt.Errorf("decode graph: %w", err)
		}
		g.Normalize()
		now := s.now()
		var schedIDs, hookIDs []string
		for i := range g.Nodes {
			n := &g.Nodes[i]
			switch n.Type {
			case workflow.TypeScheduleTrigger:
				cfgAny, err := workflow.DecodeConfig(n)
				if err != nil {
					return err
				}
				cfg := cfgAny.(*workflow.ScheduleConfig)
				tz := cfg.Timezone
				if tz == "" {
					tz = "UTC"
				}
				next, err := cron.NextIn(cfg.Cron, tz, now)
				if err != nil {
					return fmt.Errorf("node %s: %w", n.ID, err)
				}
				payload, _ := json.Marshal(cfg.Payload)
				if cfg.Payload == nil {
					payload = []byte("{}")
				}
				// Keep the pending fire time when nothing about the schedule changed.
				if _, err := tx.Exec(ctx, `INSERT INTO schedules (workflow_id, node_id, workspace_id, version_id, cron, timezone, payload, next_run_at)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
					ON CONFLICT (workflow_id, node_id) DO UPDATE SET
					  next_run_at = CASE WHEN schedules.cron = EXCLUDED.cron AND schedules.timezone = EXCLUDED.timezone
					                     THEN schedules.next_run_at ELSE EXCLUDED.next_run_at END,
					  version_id = EXCLUDED.version_id, cron = EXCLUDED.cron, timezone = EXCLUDED.timezone,
					  payload = EXCLUDED.payload, workspace_id = EXCLUDED.workspace_id, updated_at = now()`,
					workflowID, n.ID, workspaceID, versionID, cfg.Cron, tz, payload, next); err != nil {
					return err
				}
				schedIDs = append(schedIDs, n.ID)
			case workflow.TypeWebhookTrigger:
				cfgAny, err := workflow.DecodeConfig(n)
				if err != nil {
					return err
				}
				cfg := cfgAny.(*workflow.WebhookConfig)
				var secret any
				if cfg.HMACSecret != "" {
					secret = cfg.HMACSecret
				}
				if _, err := tx.Exec(ctx, `INSERT INTO webhook_endpoints (workspace_id, workflow_id, node_id, hmac_secret)
					VALUES ($1,$2,$3,$4)
					ON CONFLICT (workflow_id, node_id) DO UPDATE SET hmac_secret = EXCLUDED.hmac_secret`,
					workspaceID, workflowID, n.ID, secret); err != nil {
					return err
				}
				hookIDs = append(hookIDs, n.ID)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schedules WHERE workflow_id=$1 AND NOT (node_id = ANY($2))`, workflowID, nonNil(schedIDs)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM webhook_endpoints WHERE workflow_id=$1 AND NOT (node_id = ANY($2))`, workflowID, nonNil(hookIDs))
		return err
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Endpoint is a resolved webhook target.
type Endpoint struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	WorkflowID  string `json:"workflow_id"`
	NodeID      string `json:"node_id"`
	HMACSecret  string `json:"-"`
}

// Endpoints lists a workflow's webhook endpoints.
func (s *Store) Endpoints(ctx context.Context, workspaceID, workflowID string) ([]Endpoint, error) {
	rows, err := s.DB.Pool.Query(ctx, `SELECT id::text, workspace_id::text, workflow_id::text, node_id, COALESCE(hmac_secret,'')
		FROM webhook_endpoints WHERE workspace_id=$1 AND workflow_id=$2 ORDER BY node_id`, workspaceID, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.WorkflowID, &e.NodeID, &e.HMACSecret); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Endpoint resolves an endpoint by id; unknown or malformed ids yield ErrNoEndpoint.
func (s *Store) Endpoint(ctx context.Context, id string) (Endpoint, error) {
	var e Endpoint
	err := s.DB.Pool.QueryRow(ctx, `SELECT id::text, workspace_id::text, workflow_id::text, node_id, COALESCE(hmac_secret,'')
		FROM webhook_endpoints WHERE id::text=$1`, id).Scan(&e.ID, &e.WorkspaceID, &e.WorkflowID, &e.NodeID, &e.HMACSecret)
	if errors.Is(err, pgx.ErrNoRows) || persistence.IsInvalidText(err) {
		return e, ErrNoEndpoint
	}
	return e, err
}

// Sign computes the signature header value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifySignature compares header (with or without the sha256= prefix) in constant time.
func VerifySignature(secret string, body []byte, header string) bool {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "sha256=") {
		header = "sha256=" + header
	}
	return hmac.Equal([]byte(Sign(secret, body)), []byte(header))
}

// FireDue starts an execution for every due schedule and advances each to its
// next fire time. Rows are claimed with SKIP LOCKED, and starts carry an
// idempotency key derived from the nominal fire time, so competing scheduler
// instances (or a crash between start and commit) never double-fire.
func (s *Store) FireDue(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	fired := 0
	err := s.DB.InTx(ctx, func(tx *persistence.Tx) error {
		fired = 0 // the tx may be retried
		type due struct {
			wf, node, ws, ver, cron, tz string
			payload                     []byte
			nominal                     time.Time
		}
		rows, err := tx.Query(ctx, `SELECT s.workflow_id::text, s.node_id, s.workspace_id::text, s.version_id::text, s.cron, s.timezone, s.payload, s.next_run_at
			FROM schedules s JOIN workflows w ON w.id = s.workflow_id AND w.published_version_id = s.version_id AND w.deleted_at IS NULL
			WHERE s.next_run_at <= $1 ORDER BY s.next_run_at LIMIT $2 FOR UPDATE OF s SKIP LOCKED`, s.now(), limit)
		if err != nil {
			return err
		}
		var list []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.wf, &d.node, &d.ws, &d.ver, &d.cron, &d.tz, &d.payload, &d.nominal); err != nil {
				rows.Close()
				return err
			}
			list = append(list, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range list {
			var payload map[string]any
			_ = json.Unmarshal(d.payload, &payload)
			trig := map[string]any{"scheduled_for": d.nominal.UTC().Format(time.RFC3339), "payload": payload}
			_, err := s.RT.Start(ctx, runtime.StartParams{
				WorkspaceID: d.ws, WorkflowID: d.wf, VersionID: d.ver, TriggerType: "schedule", Trigger: trig, StartNode: d.node,
				IdempotencyKey: fmt.Sprintf("schedule:%s:%s:%d", d.wf, d.node, d.nominal.Unix()),
			})
			if err != nil && !errors.Is(err, runtime.ErrNotPublished) {
				// Leave the row due; the next tick retries with the same key.
				s.log().Warn("schedule fire failed", "workflow", d.wf, "node", d.node, "err", err)
				continue
			}
			// Skipped fire times (downtime) collapse into one run; resume from now.
			next, nerr := cron.NextIn(d.cron, d.tz, s.now())
			if nerr != nil {
				s.log().Error("schedule next", "workflow", d.wf, "node", d.node, "err", nerr)
				next = s.now().Add(24 * time.Hour)
			}
			if _, err := tx.Exec(ctx, `UPDATE schedules SET last_run_at=$3, next_run_at=$4 WHERE workflow_id=$1 AND node_id=$2`,
				d.wf, d.node, d.nominal, next); err != nil {
				return err
			}
			fired++
		}
		return nil
	})
	return fired, err
}
