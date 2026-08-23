package realtime

import (
	"fmt"
	"testing"

	"github.com/namesarnav/synapse/internal/runtime"
)

// BenchmarkDispatch measures publishing one event to subscribers of a single execution.
func BenchmarkDispatch(b *testing.B) {
	for _, subs := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			h := NewHub(1 << 20)
			for i := 0; i < subs; i++ {
				s := h.Execution("e1")
				go func() {
					for range s.C {
					}
				}()
			}
			ev := []runtime.Event{{ExecutionID: "e1", WorkspaceID: "w", Type: runtime.EvNodeStarted}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Dispatch(ev)
			}
		})
	}
}
