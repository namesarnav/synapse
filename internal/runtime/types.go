// Package runtime is the durable execution runtime. All state lives in
// PostgreSQL; every state change happens in a transaction that holds the
// execution row lock, so any process can advance any execution.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/workflow"
)

// Execution kinds.
const (
	KindRoot     = "root"
	KindForEach  = "foreach_item"
	KindSubFlow  = "sub_workflow"
	taskQueued   = "queued"
	taskLeased   = "leased"
	taskDone     = "succeeded"
	taskFailed   = "failed"
	taskCanceled = "cancelled"
	taskDead     = "dead"
)

var (
	// ErrLeaseLost means the task was reassigned or cancelled; the caller must
	// drop its result.
	ErrLeaseLost      = errors.New("lease lost")
	ErrNotPublished   = errors.New("workflow is not published")
	ErrQueueFull      = errors.New("queue is full")
	ErrTerminal       = errors.New("execution already finished")
	ErrInvalidTrigger = errors.New("invalid trigger")
)

// SecretProvider loads decrypted secrets for a workspace.
type SecretProvider interface {
	Load(ctx context.Context, workspaceID string) (map[string]string, error)
}

// Runtime is the shared handle used by the API, workers and scheduler.
type Runtime struct {
	DB  *persistence.DB
	Log *slog.Logger
	Now func() time.Time
	Rnd func() float64

	MaxDepth           int
	MaxDeliveries      int
	MaxQueueDepth      int
	DefaultNodeTimeout time.Duration
	WorkflowTimeout    time.Duration
	MaxOutputBytes     int
	LeaseDuration      time.Duration

	Secrets SecretProvider
	// OnEvents receives events after their transaction commits.
	OnEvents func([]Event)

	// DisableParentNotify skips the immediate parent wake-up so failure-injection
	// tests can exercise the sweeper.
	DisableParentNotify bool

	graphs sync.Map // version id (+scope) -> *graphInfo
}

func (rt *Runtime) defaults() {
	if rt.Now == nil {
		rt.Now = time.Now
	}
	if rt.Log == nil {
		rt.Log = slog.New(slog.DiscardHandler)
	}
	if rt.MaxDepth <= 0 {
		rt.MaxDepth = 5
	}
	if rt.MaxDeliveries <= 0 {
		rt.MaxDeliveries = 5
	}
	if rt.DefaultNodeTimeout <= 0 {
		rt.DefaultNodeTimeout = 60 * time.Second
	}
	if rt.MaxOutputBytes <= 0 {
		rt.MaxOutputBytes = 4 << 20
	}
	if rt.LeaseDuration <= 0 {
		rt.LeaseDuration = 30 * time.Second
	}
}

// New returns a Runtime with defaults applied.
func New(cfg *Runtime) *Runtime {
	rt := &Runtime{
		DB: cfg.DB, Log: cfg.Log, Now: cfg.Now, Rnd: cfg.Rnd, MaxDepth: cfg.MaxDepth, MaxDeliveries: cfg.MaxDeliveries,
		MaxQueueDepth: cfg.MaxQueueDepth, DefaultNodeTimeout: cfg.DefaultNodeTimeout, WorkflowTimeout: cfg.WorkflowTimeout,
		MaxOutputBytes: cfg.MaxOutputBytes, LeaseDuration: cfg.LeaseDuration, Secrets: cfg.Secrets, OnEvents: cfg.OnEvents, DisableParentNotify: cfg.DisableParentNotify,
	}
	rt.defaults()
	return rt
}

// Execution mirrors one executions row.
type Execution struct {
	ID               string           `json:"id"`
	WorkspaceID      string           `json:"workspace_id"`
	WorkflowID       string           `json:"workflow_id"`
	VersionID        string           `json:"version_id"`
	Version          int              `json:"version"`
	Kind             string           `json:"kind"`
	Status           engine.ExecState `json:"status"`
	TriggerType      string           `json:"trigger_type"`
	TriggerPayload   any              `json:"trigger_payload"`
	StartNode        string           `json:"start_node"`
	Output           any              `json:"output"`
	Error            string           `json:"error,omitempty"`
	ParentExecution  *string          `json:"parent_execution_id,omitempty"`
	ParentNode       string           `json:"parent_node_id,omitempty"`
	ItemIndex        *int             `json:"item_index,omitempty"`
	Depth            int              `json:"depth"`
	ReplayOf         *string          `json:"replay_of,omitempty"`
	ReplaySourceNode string           `json:"replay_source_node,omitempty"`
	IdempotencyKey   *string          `json:"idempotency_key,omitempty"`
	DeadlineAt       *time.Time       `json:"deadline_at,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	StartedAt        *time.Time       `json:"started_at,omitempty"`
	FinishedAt       *time.Time       `json:"finished_at,omitempty"`

	ctx execContext
}

// execContext is the private data a child execution inherits.
type execContext struct {
	Item  any            `json:"item,omitempty"`
	Index int            `json:"index,omitempty"`
	Nodes map[string]any `json:"nodes,omitempty"`
}

// NodeExec mirrors one node_executions row.
type NodeExec struct {
	NodeID     string            `json:"node_id"`
	NodeType   string            `json:"node_type"`
	State      engine.NodeState  `json:"state"`
	Branch     string            `json:"branch,omitempty"`
	Attempt    int               `json:"attempt"`
	Input      any               `json:"input,omitempty"`
	Output     any               `json:"output,omitempty"`
	Error      *engine.NodeError `json:"error,omitempty"`
	WakeAt     *time.Time        `json:"wake_at,omitempty"`
	WorkerID   string            `json:"worker_id,omitempty"`
	StartedAt  *time.Time        `json:"started_at,omitempty"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
}

// Event is one entry in an execution's log.
type Event struct {
	ID          int64          `json:"id"`
	ExecutionID string         `json:"execution_id"`
	WorkflowID  string         `json:"workflow_id,omitempty"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	Type        string         `json:"type"`
	NodeID      string         `json:"node_id,omitempty"`
	Attempt     int            `json:"attempt,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// Event types.
const (
	EvExecCreated   = "execution.created"
	EvExecStarted   = "execution.started"
	EvExecSucceeded = "execution.succeeded"
	EvExecFailed    = "execution.failed"
	EvExecCancelled = "execution.cancelled"
	EvExecCancelReq = "execution.cancelling"
	EvNodeQueued    = "node.queued"
	EvNodeStarted   = "node.started"
	EvNodeSucceeded = "node.succeeded"
	EvNodeFailed    = "node.failed"
	EvNodeRetrying  = "node.retrying"
	EvNodeWaiting   = "node.waiting"
	EvNodeSkipped   = "node.skipped"
	EvNodeCancelled = "node.cancelled"
	EvNodeLog       = "node.log"
)

// AllEventTypes lists every event type, used by the protocol drift check.
var AllEventTypes = []string{EvExecCreated, EvExecStarted, EvExecSucceeded, EvExecFailed, EvExecCancelled, EvExecCancelReq,
	EvNodeQueued, EvNodeStarted, EvNodeSucceeded, EvNodeFailed, EvNodeRetrying, EvNodeWaiting, EvNodeSkipped, EvNodeCancelled, EvNodeLog}

func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func unmarshalAny(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

type graphInfo struct {
	g     *workflow.Graph
	ix    *workflow.Index
	order []string
	// refs caches the node ids each node's expressions reference.
	refs map[string][]string
}

func buildGraphInfo(g workflow.Graph) (*graphInfo, error) {
	g.Normalize()
	ix := workflow.NewIndex(&g)
	order, err := ix.TopoOrder()
	if err != nil {
		return nil, err
	}
	gi := &graphInfo{g: &g, ix: ix, order: order, refs: make(map[string][]string, len(g.Nodes))}
	for i := range g.Nodes {
		gi.refs[g.Nodes[i].ID] = engine.NodeRefs(&g.Nodes[i])
	}
	return gi, nil
}
