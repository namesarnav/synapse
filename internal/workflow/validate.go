package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	SevError   = "error"
	SevWarning = "warning"

	MaxNodes = 500
	MaxEdges = 2000
)

var nodeIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// Issue is one validation finding, addressable to a node or edge so the
// editor can highlight it.
type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
}

// Report is the outcome of validating a graph.
type Report struct {
	Issues []Issue `json:"issues"`
}

func (r *Report) Errors() []Issue   { return r.filter(SevError) }
func (r *Report) Warnings() []Issue { return r.filter(SevWarning) }
func (r *Report) Valid() bool       { return len(r.Errors()) == 0 }

func (r *Report) filter(sev string) []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == sev {
			out = append(out, i)
		}
	}
	return out
}

// Has reports whether an issue with the code exists.
func (r *Report) Has(code string) bool {
	for _, i := range r.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func (r *Report) Error() string {
	var parts []string
	for _, i := range r.Errors() {
		parts = append(parts, i.Code+": "+i.Message)
	}
	return strings.Join(parts, "; ")
}

func (r *Report) add(sev, code, msg, node, edge string) {
	r.Issues = append(r.Issues, Issue{Severity: sev, Code: code, Message: msg, NodeID: node, EdgeID: edge})
}

func (r *Report) errf(code, node, edge, f string, a ...any) {
	r.add(SevError, code, fmt.Sprintf(f, a...), node, edge)
}

// Validate checks structure, node configs, expression references and the
// foreach region rule. It never panics on malformed input. ck may be nil, in
// which case expressions are not checked.
func Validate(g *Graph, ck ExprChecker) *Report {
	r := &Report{Issues: []Issue{}}
	if g == nil || len(g.Nodes) == 0 {
		r.errf("empty_graph", "", "", "workflow has no nodes")
		return r
	}
	if len(g.Nodes) > MaxNodes {
		r.errf("too_many_nodes", "", "", "workflow has %d nodes; the limit is %d", len(g.Nodes), MaxNodes)
		return r
	}
	if len(g.Edges) > MaxEdges {
		r.errf("too_many_edges", "", "", "workflow has %d edges; the limit is %d", len(g.Edges), MaxEdges)
		return r
	}

	seen := map[string]bool{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch {
		case !nodeIDRe.MatchString(n.ID) || n.ID == ItemInputID:
			r.errf("invalid_node_id", n.ID, "", "node id %q must match [A-Za-z][A-Za-z0-9_]{0,63}", n.ID)
			continue
		case seen[n.ID]:
			r.errf("duplicate_node_id", n.ID, "", "duplicate node id %q", n.ID)
			continue
		}
		seen[n.ID] = true
		if len(n.Name) > 200 {
			r.errf("invalid_name", n.ID, "", "node name is longer than 200 characters")
		}
		info, ok := Info(n.Type)
		if !ok || n.Type == TypeItemInput {
			r.errf("unknown_node_type", n.ID, "", "unknown node type %q", n.Type)
			continue
		}
		_ = info
		if n.OnError != "" && n.OnError != OnErrorFail && n.OnError != OnErrorContinue {
			r.errf("invalid_on_error", n.ID, "", "on_error must be %q or %q", OnErrorFail, OnErrorContinue)
		}
		if n.TimeoutMS < 0 || n.TimeoutMS > 3_600_000 {
			r.errf("invalid_timeout", n.ID, "", "timeout_ms must be between 0 and 3600000")
		}
		if err := n.Retry.Validate(); err != nil {
			r.errf("invalid_retry", n.ID, "", "%v", err)
		}
	}

	// edges
	edgeIDs := map[string]bool{}
	edgeKeys := map[string]bool{}
	for _, e := range g.Edges {
		if edgeIDs[e.ID] && e.ID != "" {
			r.errf("duplicate_edge_id", "", e.ID, "duplicate edge id %q", e.ID)
		}
		edgeIDs[e.ID] = true
		src, dst := lookup(g, e.Source), lookup(g, e.Target)
		if src == nil || dst == nil {
			r.errf("dangling_edge", "", e.ID, "edge %q references a missing node (%s -> %s)", e.ID, e.Source, e.Target)
			continue
		}
		if e.Source == e.Target {
			r.errf("self_loop", e.Source, e.ID, "node %q has an edge to itself", e.Source)
			continue
		}
		key := e.Source + "\x00" + e.Target + "\x00" + e.Branch
		if edgeKeys[key] {
			r.errf("duplicate_edge", "", e.ID, "duplicate edge %s -> %s", e.Source, e.Target)
		}
		edgeKeys[key] = true
		if dst.Type.IsTrigger() {
			r.errf("trigger_has_input", dst.ID, e.ID, "trigger %q cannot have incoming edges", dst.ID)
		}
		if info, ok := Info(src.Type); ok {
			if info.Terminal {
				r.errf("terminal_has_output", src.ID, e.ID, "%s node %q cannot have outgoing edges", src.Type, src.ID)
			}
			checkBranch(r, src, e, info)
		}
	}

	ix := NewIndex(g)

	// triggers
	triggers := g.Triggers()
	if len(triggers) == 0 {
		r.errf("missing_trigger", "", "", "workflow needs at least one trigger node")
	}

	// cycles
	if cyc := ix.FindCycle(); cyc != nil {
		r.errf("cycle", cyc[0], "", "cycle detected: %s", strings.Join(cyc, " -> "))
	}

	// unreachable
	if len(triggers) > 0 {
		starts := make([]string, len(triggers))
		for i, t := range triggers {
			starts[i] = t.ID
		}
		reach := ix.Reachable(starts, nil)
		for _, n := range g.Nodes {
			if ix.Nodes[n.ID] != nil && !reach[n.ID] {
				r.errf("unreachable_node", n.ID, "", "node %q is not reachable from any trigger", n.ID)
			}
		}
	}

	// per-node config, merge shape, foreach regions
	acyclic := !r.Has("cycle")
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if ix.Nodes[n.ID] != n {
			continue
		}
		if _, ok := Info(n.Type); !ok || n.Type == TypeItemInput {
			continue
		}
		problems, refs := checkConfig(n, ck)
		for _, p := range problems {
			r.errf("invalid_config", n.ID, "", "%s: %s", n.ID, p)
		}
		if acyclic && len(refs) > 0 {
			anc := ix.Ancestors(n.ID)
			for _, ref := range uniq(refs) {
				switch {
				case ix.Nodes[ref] == nil:
					r.errf("unknown_reference", n.ID, "", "%s references unknown node %q", n.ID, ref)
				case !anc[ref]:
					r.errf("reference_not_ancestor", n.ID, "", "%s references %q which is not upstream of it", n.ID, ref)
				}
			}
		}
		if n.Type == TypeMerge && len(ix.In[n.ID]) < 2 {
			r.add(SevWarning, "merge_single_input", fmt.Sprintf("merge %q has fewer than two inputs", n.ID), n.ID, "")
		}
		if n.Type == TypeForEach && acyclic {
			checkForEach(r, ix, n)
		}
	}
	sort.SliceStable(r.Issues, func(i, j int) bool {
		if r.Issues[i].Severity != r.Issues[j].Severity {
			return r.Issues[i].Severity == SevError
		}
		return false
	})
	return r
}

func lookup(g *Graph, id string) *Node {
	n, _ := g.Node(id)
	return n
}

func checkBranch(r *Report, src *Node, e Edge, info TypeInfo) {
	if len(info.Branches) == 0 {
		if e.Branch != "" {
			r.errf("invalid_branch", src.ID, e.ID, "%s node %q does not support branch %q", src.Type, src.ID, e.Branch)
		}
		return
	}
	for _, b := range info.Branches {
		if b == e.Branch {
			return
		}
	}
	r.errf("invalid_branch", src.ID, e.ID, "%s node %q edge must be one of %v, got %q", src.Type, src.ID, info.Branches, e.Branch)
}

// checkForEach enforces the region rule: the body is single-entry (only the
// foreach's item edges lead in) and single-exit (nothing leaves the body).
func checkForEach(r *Report, ix *Index, n *Node) {
	var items int
	for _, e := range ix.Out[n.ID] {
		if e.Branch == BranchItem {
			items++
		}
	}
	if items == 0 {
		r.errf("foreach_no_body", n.ID, "", "foreach %q needs at least one item edge", n.ID)
		return
	}
	body := ix.ForEachBody(n.ID)
	if body[n.ID] {
		r.errf("foreach_region", n.ID, "", "foreach %q body loops back to itself", n.ID)
		return
	}
	cont := ix.ForEachContinuation(n.ID)
	for id := range body {
		node := ix.Nodes[id]
		if node.Type.IsTrigger() {
			r.errf("foreach_region", id, "", "trigger %q cannot be inside foreach %q", id, n.ID)
		}
		if cont[id] {
			r.errf("foreach_region", id, "", "node %q is reachable from both the item and done branches of foreach %q", id, n.ID)
		}
		for _, e := range ix.In[id] {
			if e.Source == n.ID && e.Branch == BranchItem {
				continue
			}
			if !body[e.Source] {
				r.errf("foreach_region", id, e.ID, "node %q inside foreach %q has an incoming edge from outside the loop body (%s)", id, n.ID, e.Source)
			}
		}
		for _, e := range ix.Out[id] {
			if !body[e.Target] {
				r.errf("foreach_region", id, e.ID, "node %q inside foreach %q has an edge leaving the loop body (%s)", id, n.ID, e.Target)
			}
		}
	}
}

func uniq(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
