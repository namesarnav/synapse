// Package workflow defines the workflow graph model, its validation and the
// graph algorithms the engine relies on. It has no database or runtime deps.
package workflow

import (
	"encoding/json"
	"sort"
)

type NodeType string

const (
	TypeWebhookTrigger  NodeType = "webhook_trigger"
	TypeScheduleTrigger NodeType = "schedule_trigger"
	TypeManualTrigger   NodeType = "manual_trigger"
	TypeCondition       NodeType = "condition"
	TypeDelay           NodeType = "delay"
	TypeForEach         NodeType = "foreach"
	TypeTransform       NodeType = "transform"
	TypeMerge           NodeType = "merge"
	TypeStop            NodeType = "stop"
	TypeHTTPRequest     NodeType = "http_request"
	TypeLog             NodeType = "log"
	TypeEmail           NodeType = "email"
	TypeSubWorkflow     NodeType = "sub_workflow"

	// TypeItemInput is synthesised as the entry of a foreach body; it is never
	// authored by users.
	TypeItemInput NodeType = "item_input"
)

// Branch labels on edges.
const (
	BranchTrue  = "true"
	BranchFalse = "false"
	BranchItem  = "item"
	BranchDone  = "done"
)

// Category groups node types in the editor palette.
type Category string

const (
	CatTrigger Category = "trigger"
	CatLogic   Category = "logic"
	CatAction  Category = "action"
)

// TypeInfo is the static description of a node type.
type TypeInfo struct {
	Type     NodeType `json:"type"`
	Label    string   `json:"label"`
	Category Category `json:"category"`
	// Branches lists the edge labels a node of this type may emit. Empty means
	// only unlabeled edges.
	Branches []string `json:"branches,omitempty"`
	// Inline nodes are executed by the engine inside a state transition; the
	// rest are dispatched to workers as tasks.
	Inline bool `json:"inline"`
	// Terminal nodes have no outgoing edges.
	Terminal bool `json:"terminal,omitempty"`
}

var catalog = []TypeInfo{
	{Type: TypeWebhookTrigger, Label: "Webhook", Category: CatTrigger, Inline: true},
	{Type: TypeScheduleTrigger, Label: "Schedule", Category: CatTrigger, Inline: true},
	{Type: TypeManualTrigger, Label: "Manual", Category: CatTrigger, Inline: true},
	{Type: TypeCondition, Label: "Condition", Category: CatLogic, Branches: []string{BranchTrue, BranchFalse}, Inline: true},
	{Type: TypeDelay, Label: "Delay", Category: CatLogic, Inline: true},
	{Type: TypeForEach, Label: "ForEach", Category: CatLogic, Branches: []string{BranchItem, BranchDone}, Inline: true},
	{Type: TypeTransform, Label: "Transform", Category: CatLogic},
	{Type: TypeMerge, Label: "Merge", Category: CatLogic, Inline: true},
	{Type: TypeStop, Label: "Stop", Category: CatLogic, Inline: true, Terminal: true},
	{Type: TypeHTTPRequest, Label: "HTTP Request", Category: CatAction},
	{Type: TypeLog, Label: "Log", Category: CatAction},
	{Type: TypeEmail, Label: "Email", Category: CatAction},
	{Type: TypeSubWorkflow, Label: "Sub-workflow", Category: CatAction, Inline: true},
	{Type: TypeItemInput, Label: "Item input", Category: CatTrigger, Inline: true},
}

var catalogByType = func() map[NodeType]TypeInfo {
	m := make(map[NodeType]TypeInfo, len(catalog))
	for _, t := range catalog {
		m[t.Type] = t
	}
	return m
}()

// Catalog returns all authorable node types.
func Catalog() []TypeInfo {
	out := make([]TypeInfo, 0, len(catalog))
	for _, t := range catalog {
		if t.Type != TypeItemInput {
			out = append(out, t)
		}
	}
	return out
}

func Info(t NodeType) (TypeInfo, bool) { i, ok := catalogByType[t]; return i, ok }

func (t NodeType) IsTrigger() bool {
	i, ok := catalogByType[t]
	return ok && i.Category == CatTrigger
}

func (t NodeType) IsInline() bool { return catalogByType[t].Inline }

// OnError policies for a failed node.
const (
	OnErrorFail     = "fail"
	OnErrorContinue = "continue"
)

type Node struct {
	ID        string          `json:"id"`
	Type      NodeType        `json:"type"`
	Name      string          `json:"name,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	Retry     *RetryPolicy    `json:"retry,omitempty"`
	TimeoutMS int             `json:"timeout_ms,omitempty"`
	OnError   string          `json:"on_error,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"` // editor data such as position
}

type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Branch string `json:"branch,omitempty"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node returns the node with the given id.
func (g *Graph) Node(id string) (*Node, bool) {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i], true
		}
	}
	return nil, false
}

// Triggers returns trigger nodes in a stable order.
func (g *Graph) Triggers() []Node {
	var out []Node
	for _, n := range g.Nodes {
		if n.Type.IsTrigger() {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Normalize fills defaults so that stored graphs are canonical.
func (g *Graph) Normalize() {
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.OnError == "" {
			n.OnError = OnErrorFail
		}
		if len(n.Config) == 0 || string(n.Config) == "null" {
			n.Config = json.RawMessage(`{}`)
		}
	}
	types := make(map[string]NodeType, len(g.Nodes))
	for _, n := range g.Nodes {
		types[n.ID] = n.Type
	}
	for i := range g.Edges {
		if g.Edges[i].Branch == "default" {
			g.Edges[i].Branch = ""
		}
		if g.Edges[i].Branch == "" && types[g.Edges[i].Source] == TypeForEach {
			g.Edges[i].Branch = BranchDone
		}
		if g.Edges[i].ID == "" {
			g.Edges[i].ID = g.Edges[i].Source + "->" + g.Edges[i].Target + ":" + g.Edges[i].Branch
		}
	}
}
