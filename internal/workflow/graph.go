package workflow

import (
	"fmt"
	"sort"
)

// Index is an adjacency view of a graph. Edges whose endpoints are missing are
// kept in Edges but skipped in adjacency lists, so Index is safe to build from
// an unvalidated graph.
type Index struct {
	Graph *Graph
	Nodes map[string]*Node
	Out   map[string][]Edge
	In    map[string][]Edge
}

func NewIndex(g *Graph) *Index {
	ix := &Index{Graph: g, Nodes: make(map[string]*Node, len(g.Nodes)), Out: map[string][]Edge{}, In: map[string][]Edge{}}
	for i := range g.Nodes {
		if _, dup := ix.Nodes[g.Nodes[i].ID]; !dup {
			ix.Nodes[g.Nodes[i].ID] = &g.Nodes[i]
		}
	}
	for _, e := range g.Edges {
		if ix.Nodes[e.Source] == nil || ix.Nodes[e.Target] == nil {
			continue
		}
		ix.Out[e.Source] = append(ix.Out[e.Source], e)
		ix.In[e.Target] = append(ix.In[e.Target], e)
	}
	return ix
}

// InDegrees returns the number of incoming edges per node (dependency counts).
func (ix *Index) InDegrees() map[string]int {
	d := make(map[string]int, len(ix.Nodes))
	for id := range ix.Nodes {
		d[id] = len(ix.In[id])
	}
	return d
}

// CycleError reports a directed cycle as an ordered path a -> b -> ... -> a.
type CycleError struct{ Path []string }

func (e *CycleError) Error() string { return fmt.Sprintf("cycle detected: %v", e.Path) }

// FindCycle returns one directed cycle using iterative three-colour DFS, or nil.
func (ix *Index) FindCycle() []string {
	const (
		white = iota
		grey
		black
	)
	color := make(map[string]int, len(ix.Nodes))
	parent := map[string]string{}
	ids := ix.sortedIDs()
	type frame struct {
		id string
		i  int
	}
	for _, root := range ids {
		if color[root] != white {
			continue
		}
		stack := []frame{{root, 0}}
		color[root] = grey
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			out := ix.Out[f.id]
			if f.i >= len(out) {
				color[f.id] = black
				stack = stack[:len(stack)-1]
				continue
			}
			next := out[f.i].Target
			f.i++
			switch color[next] {
			case white:
				color[next] = grey
				parent[next] = f.id
				stack = append(stack, frame{next, 0})
			case grey: // back edge: unwind parents from f.id to next
				path := []string{next}
				for cur := f.id; cur != next; cur = parent[cur] {
					path = append(path, cur)
				}
				path = append(path, next)
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return path
			}
		}
	}
	return nil
}

// TopoOrder returns a topological ordering using Kahn's algorithm with a
// deterministic tie-break (sorted IDs). It returns a *CycleError if the graph
// is not a DAG.
func (ix *Index) TopoOrder() ([]string, error) {
	deg := ix.InDegrees()
	var ready []string
	for _, id := range ix.sortedIDs() {
		if deg[id] == 0 {
			ready = append(ready, id)
		}
	}
	order := make([]string, 0, len(ix.Nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		var next []string
		for _, e := range ix.Out[id] {
			deg[e.Target]--
			if deg[e.Target] == 0 {
				next = append(next, e.Target)
			}
		}
		sort.Strings(next)
		ready = append(ready, next...)
	}
	if len(order) != len(ix.Nodes) {
		return order, &CycleError{Path: ix.FindCycle()}
	}
	return order, nil
}

// Levels groups nodes by longest-path depth: every node in level k depends
// only on nodes in earlier levels. Nodes inside a level may run concurrently.
func (ix *Index) Levels() ([][]string, error) {
	order, err := ix.TopoOrder()
	if err != nil {
		return nil, err
	}
	depth := map[string]int{}
	maxd := 0
	for _, id := range order {
		d := 0
		for _, e := range ix.In[id] {
			if depth[e.Source]+1 > d {
				d = depth[e.Source] + 1
			}
		}
		depth[id] = d
		if d > maxd {
			maxd = d
		}
	}
	levels := make([][]string, maxd+1)
	for _, id := range order {
		levels[depth[id]] = append(levels[depth[id]], id)
	}
	if len(order) == 0 {
		return nil, nil
	}
	return levels, nil
}

// Reachable returns the set of nodes reachable from starts (starts included),
// following only edges accepted by follow (nil accepts all). BFS.
func (ix *Index) Reachable(starts []string, follow func(Edge) bool) map[string]bool {
	seen := map[string]bool{}
	queue := make([]string, 0, len(starts))
	for _, s := range starts {
		if ix.Nodes[s] != nil && !seen[s] {
			seen[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range ix.Out[id] {
			if follow != nil && !follow(e) {
				continue
			}
			if !seen[e.Target] {
				seen[e.Target] = true
				queue = append(queue, e.Target)
			}
		}
	}
	return seen
}

// Descendants returns every node strictly downstream of id.
func (ix *Index) Descendants(id string) map[string]bool {
	r := ix.Reachable([]string{id}, nil)
	delete(r, id)
	return r
}

// Ancestors returns every node strictly upstream of id (reverse DFS).
func (ix *Index) Ancestors(id string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range ix.In[cur] {
			if !seen[e.Source] {
				seen[e.Source] = true
				stack = append(stack, e.Source)
			}
		}
	}
	return seen
}

// ForEachBody returns the node IDs in the body of a foreach: everything
// reachable through its "item" edges.
func (ix *Index) ForEachBody(id string) map[string]bool {
	var starts []string
	for _, e := range ix.Out[id] {
		if e.Branch == BranchItem {
			starts = append(starts, e.Target)
		}
	}
	return ix.Reachable(starts, nil)
}

// ForEachContinuation returns nodes reachable through the "done" (or
// unlabeled) edges of a foreach.
func (ix *Index) ForEachContinuation(id string) map[string]bool {
	var starts []string
	for _, e := range ix.Out[id] {
		if e.Branch != BranchItem {
			starts = append(starts, e.Target)
		}
	}
	return ix.Reachable(starts, nil)
}

// ExtractBody builds the subgraph executed once per foreach item. A synthetic
// item_input node feeds the body's entry nodes.
func ExtractBody(g *Graph, foreachID string) Graph {
	ix := NewIndex(g)
	body := ix.ForEachBody(foreachID)
	out := Graph{Nodes: []Node{{ID: ItemInputID, Type: TypeItemInput, Config: []byte(`{}`), OnError: OnErrorFail}}}
	for _, n := range g.Nodes {
		if body[n.ID] {
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		switch {
		case e.Source == foreachID && e.Branch == BranchItem && body[e.Target]:
			out.Edges = append(out.Edges, Edge{ID: ItemInputID + "->" + e.Target, Source: ItemInputID, Target: e.Target})
		case body[e.Source] && body[e.Target]:
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// ItemInputID is the id of the synthetic foreach body entry node.
const ItemInputID = "__item"

// BodySinks returns body nodes with no outgoing edge inside the body; their
// outputs form an iteration's result.
func (ix *Index) BodySinks(body map[string]bool) []string {
	var sinks []string
	for id := range body {
		has := false
		for _, e := range ix.Out[id] {
			if body[e.Target] {
				has = true
				break
			}
		}
		if !has {
			sinks = append(sinks, id)
		}
	}
	sort.Strings(sinks)
	return sinks
}

func (ix *Index) sortedIDs() []string {
	ids := make([]string, 0, len(ix.Nodes))
	for id := range ix.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
