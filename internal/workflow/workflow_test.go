package workflow

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func node(id string, t NodeType, cfg string) Node {
	n := Node{ID: id, Type: t}
	if cfg != "" {
		n.Config = json.RawMessage(cfg)
	}
	return n
}

func edge(src, dst string, branch ...string) Edge {
	e := Edge{Source: src, Target: dst}
	if len(branch) > 0 {
		e.Branch = branch[0]
	}
	return e
}

func build(nodes []Node, edges ...Edge) *Graph {
	g := &Graph{Nodes: nodes, Edges: edges}
	g.Normalize()
	return g
}

func linear() *Graph {
	return build(
		[]Node{
			node("t", TypeManualTrigger, ""),
			node("a", TypeLog, `{"message":"hi"}`),
			node("b", TypeLog, `{"message":"bye"}`),
		},
		edge("t", "a"), edge("a", "b"),
	)
}

func codes(r *Report) []string {
	var out []string
	for _, i := range r.Issues {
		out = append(out, i.Code)
	}
	return out
}

func TestValidateOK(t *testing.T) {
	r := Validate(linear(), nil)
	if !r.Valid() {
		t.Fatalf("expected valid, got %v", r.Issues)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		g    *Graph
		want string
	}{
		{"empty", &Graph{}, "empty_graph"},
		{"missing trigger", build([]Node{node("a", TypeLog, `{"message":"x"}`)}), "missing_trigger"},
		{"duplicate node", build([]Node{node("t", TypeManualTrigger, ""), node("t", TypeManualTrigger, "")}), "duplicate_node_id"},
		{"bad id", build([]Node{node("has space", TypeManualTrigger, "")}), "invalid_node_id"},
		{"reserved id", build([]Node{node(ItemInputID, TypeManualTrigger, "")}), "invalid_node_id"},
		{"unknown type", build([]Node{node("t", TypeManualTrigger, ""), node("x", "bogus", "")}, edge("t", "x")), "unknown_node_type"},
		{"dangling", build([]Node{node("t", TypeManualTrigger, "")}, edge("t", "ghost")), "dangling_edge"},
		{"self loop", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x"}`)}, edge("t", "a"), edge("a", "a")), "self_loop"},
		{"duplicate edge", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x"}`)},
			Edge{ID: "e1", Source: "t", Target: "a"}, Edge{ID: "e2", Source: "t", Target: "a"}), "duplicate_edge"},
		{"cycle", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x"}`), node("b", TypeLog, `{"message":"x"}`)},
			edge("t", "a"), edge("a", "b"), edge("b", "a")), "cycle"},
		{"unreachable", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x"}`)}), "unreachable_node"},
		{"trigger with input", build([]Node{node("t", TypeManualTrigger, ""), node("u", TypeManualTrigger, "")}, edge("t", "u")), "trigger_has_input"},
		{"stop with output", build([]Node{node("t", TypeManualTrigger, ""), node("s", TypeStop, ""), node("a", TypeLog, `{"message":"x"}`)}, edge("t", "s"), edge("s", "a")), "terminal_has_output"},
		{"branch on non-branching", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x"}`)}, edge("t", "a", "true")), "invalid_branch"},
		{"condition unlabeled", build([]Node{node("t", TypeManualTrigger, ""), node("c", TypeCondition, `{"expression":"true"}`), node("a", TypeLog, `{"message":"x"}`)},
			edge("t", "c"), edge("c", "a")), "invalid_branch"},
		{"bad config field", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeLog, `{"message":"x","bogus":1}`)}, edge("t", "a")), "invalid_config"},
		{"http bad url", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeHTTPRequest, `{"url":"ftp://x"}`)}, edge("t", "a")), "invalid_config"},
		{"delay none", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeDelay, `{}`)}, edge("t", "a")), "invalid_config"},
		{"delay too long", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeDelay, `{"duration":"1000h"}`)}, edge("t", "a")), "invalid_config"},
		{"transform empty", build([]Node{node("t", TypeManualTrigger, ""), node("a", TypeTransform, `{}`)}, edge("t", "a")), "invalid_config"},
		{"bad retry", func() *Graph {
			g := linear()
			g.Nodes[1].Retry = &RetryPolicy{MaxAttempts: 500}
			return g
		}(), "invalid_retry"},
		{"bad timeout", func() *Graph {
			g := linear()
			g.Nodes[1].TimeoutMS = -1
			return g
		}(), "invalid_timeout"},
		{"bad on_error", func() *Graph {
			g := linear()
			g.Nodes[1].OnError = "explode"
			return g
		}(), "invalid_on_error"},
		{"foreach no body", build([]Node{node("t", TypeManualTrigger, ""), node("f", TypeForEach, `{"items":"[1]"}`), node("a", TypeLog, `{"message":"x"}`)},
			edge("t", "f"), edge("f", "a")), "foreach_no_body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(tc.g, nil)
			if r.Valid() || !r.Has(tc.want) {
				t.Fatalf("want error %q, got %v", tc.want, codes(r))
			}
		})
	}
}

func TestValidateTooLarge(t *testing.T) {
	g := &Graph{}
	for i := 0; i <= MaxNodes; i++ {
		g.Nodes = append(g.Nodes, node(fmt.Sprintf("n%d", i), TypeLog, `{"message":"x"}`))
	}
	if r := Validate(g, nil); !r.Has("too_many_nodes") {
		t.Fatal("expected too_many_nodes")
	}
}

func TestForEachRegionRule(t *testing.T) {
	nodes := []Node{
		node("t", TypeManualTrigger, ""),
		node("f", TypeForEach, `{"items":"[1,2]"}`),
		node("body", TypeLog, `{"message":"x"}`),
		node("after", TypeLog, `{"message":"x"}`),
		node("other", TypeLog, `{"message":"x"}`),
	}
	ok := build(nodes, edge("t", "f"), edge("f", "body", BranchItem), edge("f", "after", BranchDone), edge("t", "other"))
	if r := Validate(ok, nil); !r.Valid() {
		t.Fatalf("valid foreach rejected: %v", r.Issues)
	}
	leak := build(nodes, edge("t", "f"), edge("f", "body", BranchItem), edge("f", "after", BranchDone), edge("body", "after"))
	if r := Validate(leak, nil); !r.Has("foreach_region") {
		t.Fatalf("body leaking out should fail: %v", codes(r))
	}
	enter := build(nodes, edge("t", "f"), edge("f", "body", BranchItem), edge("f", "after", BranchDone), edge("other", "body"), edge("t", "other"))
	if r := Validate(enter, nil); !r.Has("foreach_region") {
		t.Fatalf("outside entering body should fail: %v", codes(r))
	}
}

func TestExtractBody(t *testing.T) {
	g := build([]Node{
		node("t", TypeManualTrigger, ""),
		node("f", TypeForEach, `{"items":"[1]"}`),
		node("b1", TypeLog, `{"message":"x"}`),
		node("b2", TypeLog, `{"message":"x"}`),
		node("after", TypeLog, `{"message":"x"}`),
	}, edge("t", "f"), edge("f", "b1", BranchItem), edge("b1", "b2"), edge("f", "after", BranchDone))
	body := ExtractBody(g, "f")
	if len(body.Nodes) != 3 || body.Nodes[0].ID != ItemInputID {
		t.Fatalf("body nodes: %+v", body.Nodes)
	}
	ix := NewIndex(&body)
	order, err := ix.TopoOrder()
	if err != nil || !reflect.DeepEqual(order, []string{ItemInputID, "b1", "b2"}) {
		t.Fatalf("order %v err %v", order, err)
	}
	full := NewIndex(g)
	sinks := full.BodySinks(full.ForEachBody("f"))
	if !reflect.DeepEqual(sinks, []string{"b2"}) {
		t.Fatalf("sinks %v", sinks)
	}
}

func TestNormalizeForEachDefaultBranch(t *testing.T) {
	g := build([]Node{node("f", TypeForEach, `{"items":"x"}`), node("a", TypeLog, "")}, edge("f", "a"))
	if g.Edges[0].Branch != BranchDone {
		t.Fatalf("branch %q", g.Edges[0].Branch)
	}
}

func TestTopoAndLevels(t *testing.T) {
	// diamond: t -> a,b -> m
	g := build([]Node{
		node("t", TypeManualTrigger, ""), node("a", TypeLog, ""), node("b", TypeLog, ""), node("m", TypeMerge, ""),
	}, edge("t", "a"), edge("t", "b"), edge("a", "m"), edge("b", "m"))
	ix := NewIndex(g)
	order, err := ix.TopoOrder()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"t", "a", "b", "m"}) {
		t.Fatalf("order %v", order)
	}
	lv, _ := ix.Levels()
	if !reflect.DeepEqual(lv, [][]string{{"t"}, {"a", "b"}, {"m"}}) {
		t.Fatalf("levels %v", lv)
	}
	if d := ix.InDegrees(); d["m"] != 2 || d["t"] != 0 {
		t.Fatalf("degrees %v", d)
	}
	if got := ix.Ancestors("m"); len(got) != 3 {
		t.Fatalf("ancestors %v", got)
	}
	if got := ix.Descendants("t"); len(got) != 3 {
		t.Fatalf("descendants %v", got)
	}
}

func TestCyclePath(t *testing.T) {
	g := build([]Node{node("a", TypeLog, ""), node("b", TypeLog, ""), node("c", TypeLog, "")},
		edge("a", "b"), edge("b", "c"), edge("c", "a"))
	ix := NewIndex(g)
	cyc := ix.FindCycle()
	if len(cyc) != 4 || cyc[0] != cyc[len(cyc)-1] {
		t.Fatalf("cycle %v", cyc)
	}
	if _, err := ix.TopoOrder(); err == nil {
		t.Fatal("topo should fail on cycle")
	}
}

// randomDAG builds a DAG by only adding edges from lower to higher index.
func randomDAG(rng *rand.Rand, n int, density float64) *Graph {
	g := &Graph{}
	for i := 0; i < n; i++ {
		g.Nodes = append(g.Nodes, Node{ID: fmt.Sprintf("n%03d", i), Type: TypeLog})
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if rng.Float64() < density {
				g.Edges = append(g.Edges, Edge{Source: g.Nodes[i].ID, Target: g.Nodes[j].ID})
			}
		}
	}
	return g
}

func TestRandomDAGsTopoInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(40)
		g := randomDAG(rng, n, rng.Float64()*0.4)
		ix := NewIndex(g)
		if c := ix.FindCycle(); c != nil {
			t.Fatalf("iter %d: false cycle %v", iter, c)
		}
		order, err := ix.TopoOrder()
		if err != nil || len(order) != n {
			t.Fatalf("iter %d: topo err=%v len=%d", iter, err, len(order))
		}
		pos := map[string]int{}
		for i, id := range order {
			pos[id] = i
		}
		for _, e := range g.Edges {
			if pos[e.Source] >= pos[e.Target] {
				t.Fatalf("iter %d: edge %s->%s violates order", iter, e.Source, e.Target)
			}
		}
		lv, _ := ix.Levels()
		level := map[string]int{}
		for i, l := range lv {
			for _, id := range l {
				level[id] = i
			}
		}
		for _, e := range g.Edges {
			if level[e.Source] >= level[e.Target] {
				t.Fatalf("iter %d: level violation %s->%s", iter, e.Source, e.Target)
			}
		}
		// reachability matches ancestors/descendants duality
		for _, nd := range g.Nodes {
			for d := range ix.Descendants(nd.ID) {
				if !ix.Ancestors(d)[nd.ID] {
					t.Fatalf("iter %d: %s desc %s but not ancestor", iter, nd.ID, d)
				}
			}
		}
	}
}

func TestRandomGraphsWithBackEdgeHaveCycle(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 200; iter++ {
		n := 2 + rng.Intn(30)
		g := randomDAG(rng, n, 0.3)
		// chain guarantees a path 0->...->k so a back edge k->0 makes a cycle
		for i := 0; i+1 < n; i++ {
			g.Edges = append(g.Edges, Edge{Source: g.Nodes[i].ID, Target: g.Nodes[i+1].ID})
		}
		g.Edges = append(g.Edges, Edge{Source: g.Nodes[n-1].ID, Target: g.Nodes[0].ID})
		ix := NewIndex(g)
		c := ix.FindCycle()
		if c == nil {
			t.Fatalf("iter %d: cycle not found", iter)
		}
		// verify returned path edges exist
		has := map[[2]string]bool{}
		for _, e := range g.Edges {
			has[[2]string{e.Source, e.Target}] = true
		}
		for i := 0; i+1 < len(c); i++ {
			if !has[[2]string{c[i], c[i+1]}] {
				t.Fatalf("iter %d: bogus cycle edge %s->%s in %v", iter, c[i], c[i+1], c)
			}
		}
		if _, err := ix.TopoOrder(); err == nil {
			t.Fatalf("iter %d: topo accepted cycle", iter)
		}
	}
}

func TestValidateNeverPanicsOnGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	types := []NodeType{TypeLog, TypeCondition, TypeForEach, TypeMerge, TypeStop, TypeManualTrigger, "nope"}
	for iter := 0; iter < 500; iter++ {
		g := &Graph{}
		n := rng.Intn(12)
		for i := 0; i < n; i++ {
			g.Nodes = append(g.Nodes, Node{ID: fmt.Sprintf("n%d", rng.Intn(n+2)), Type: types[rng.Intn(len(types))], Config: json.RawMessage(`{"x":`)})
		}
		for i := 0; i < rng.Intn(20); i++ {
			g.Edges = append(g.Edges, Edge{ID: fmt.Sprintf("e%d", rng.Intn(5)), Source: fmt.Sprintf("n%d", rng.Intn(n+2)), Target: fmt.Sprintf("n%d", rng.Intn(n+2)), Branch: []string{"", "true", "item"}[rng.Intn(3)]})
		}
		_ = Validate(g, nil)
	}
}

func TestRetryPolicy(t *testing.T) {
	p := &RetryPolicy{MaxAttempts: 4, InitialDelayMS: 100, MaxDelayMS: 500, Multiplier: 2}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond}
	for i, w := range want {
		if got := p.Delay(i+1, nil); got != w {
			t.Errorf("attempt %d: got %v want %v", i+1, got, w)
		}
	}
	if !p.CanRetry(3) || p.CanRetry(4) {
		t.Fatal("CanRetry boundaries wrong")
	}
	var nilp *RetryPolicy
	if nilp.CanRetry(1) {
		t.Fatal("nil policy must not retry")
	}
	j := &RetryPolicy{MaxAttempts: 3, InitialDelayMS: 1000, Multiplier: 1, Jitter: 0.5}
	lo, hi := j.Delay(1, func() float64 { return 0 }), j.Delay(1, func() float64 { return 0.999999 })
	if lo != 500*time.Millisecond || hi < 1490*time.Millisecond {
		t.Fatalf("jitter bounds lo=%v hi=%v", lo, hi)
	}
	if err := (&RetryPolicy{Jitter: 2}).Validate(); err == nil {
		t.Fatal("jitter>1 must be invalid")
	}
}

type fakeChecker struct{ refs map[string][]string }

func (f fakeChecker) CheckExpression(s string) ([]string, error) { return f.refs[s], nil }
func (f fakeChecker) CheckTemplate(s string) ([]string, error)   { return f.refs[s], nil }
func (f fakeChecker) CheckCron(string, string) error             { return nil }

func TestReferenceChecks(t *testing.T) {
	g := build([]Node{
		node("t", TypeManualTrigger, ""),
		node("a", TypeLog, `{"message":"x"}`),
		node("c", TypeCondition, `{"expression":"E1"}`),
		node("later", TypeLog, `{"message":"x"}`),
	}, edge("t", "a"), edge("a", "c"), edge("c", "later", "true"))
	ok := Validate(g, fakeChecker{refs: map[string][]string{"E1": {"a"}}})
	if !ok.Valid() {
		t.Fatalf("upstream ref rejected: %v", ok.Issues)
	}
	unk := Validate(g, fakeChecker{refs: map[string][]string{"E1": {"nope"}}})
	if !unk.Has("unknown_reference") {
		t.Fatalf("got %v", codes(unk))
	}
	down := Validate(g, fakeChecker{refs: map[string][]string{"E1": {"later"}}})
	if !down.Has("reference_not_ancestor") {
		t.Fatalf("got %v", codes(down))
	}
}

func TestDecodeConfigStrict(t *testing.T) {
	n := &Node{Type: TypeHTTPRequest, Config: json.RawMessage(`{"url":"https://x.test","method":"POST"}`)}
	c, err := DecodeConfig(n)
	if err != nil || c.(*HTTPConfig).Method != "POST" {
		t.Fatalf("%v %v", c, err)
	}
	n.Config = json.RawMessage(`{"url":"x","wat":1}`)
	if _, err := DecodeConfig(n); err == nil {
		t.Fatal("unknown field must error")
	}
}

func TestCatalog(t *testing.T) {
	for _, ti := range Catalog() {
		if ti.Type == TypeItemInput {
			t.Fatal("item_input must not be in the public catalog")
		}
	}
	if len(Catalog()) != 13 {
		t.Fatalf("catalog size %d", len(Catalog()))
	}
}
