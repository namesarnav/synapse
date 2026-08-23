package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/namesarnav/synapse/internal/workflow"
)

// layered builds `layers` layers of `width` transform nodes, each fully connected to the next layer's node of the same column.
func layered(layers, width int) (*workflow.Index, []string) {
	g := &workflow.Graph{Nodes: []workflow.Node{{ID: "t", Type: "manual_trigger", Config: json.RawMessage(`{}`)}}}
	for l := 0; l < layers; l++ {
		for c := 0; c < width; c++ {
			id := fmt.Sprintf("n%d_%d", l, c)
			g.Nodes = append(g.Nodes, workflow.Node{ID: id, Type: "transform", Config: json.RawMessage(`{}`), OnError: workflow.OnErrorFail})
			src := "t"
			if l > 0 {
				src = fmt.Sprintf("n%d_%d", l-1, c)
			}
			g.Edges = append(g.Edges, workflow.Edge{ID: "e" + id, Source: src, Target: id})
		}
	}
	ix := workflow.NewIndex(g)
	order, _ := ix.TopoOrder()
	return ix, order
}

// BenchmarkResolve measures one scheduling decision over a 200-node graph
// where the first half of the layers has finished.
func BenchmarkResolve(b *testing.B) {
	ix, order := layered(20, 10)
	st := Statuses{"t": {State: NodeSucceeded, OnError: workflow.OnErrorFail}}
	for i, id := range order {
		if id == "t" {
			continue
		}
		s := NodePending
		if i <= len(order)/2 {
			s = NodeSucceeded
		}
		st[id] = &Status{State: s, OnError: workflow.OnErrorFail}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Resolve(ix, order, st)
	}
}
