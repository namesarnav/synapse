package realtime

import "github.com/namesarnav/synapse/internal/runtime"

// Server-to-client message types. The TypeScript mirror lives in
// apps/web/src/api/protocol.ts and a test keeps the two in sync.
const (
	MsgHello  = "hello"  // sent once after subscribing
	MsgEvent  = "event"  // one execution event
	MsgResync = "resync" // client fell behind; reconnect with ?after=<last event id>
	MsgEnd    = "end"    // execution finished and every event was delivered
	MsgError  = "error"
)

// Close codes used by the gateway.
const (
	CloseNormal     = 1000
	CloseGoingAway  = 1001
	CloseSlow       = 1013 // try again later: the client was too slow
	CloseOverloaded = 1013
)

// Message is the envelope for every server-to-client frame.
type Message struct {
	Type        string         `json:"type"`
	Event       *runtime.Event `json:"event,omitempty"`
	ExecutionID string         `json:"execution_id,omitempty"`
	Status      string         `json:"status,omitempty"`
	LastEventID int64          `json:"last_event_id,omitempty"`
	Code        string         `json:"code,omitempty"`
	Message     string         `json:"message,omitempty"`
}

// MessageTypes lists every message type, used by the protocol drift test.
var MessageTypes = []string{MsgHello, MsgEvent, MsgResync, MsgEnd, MsgError}

// IsTerminal reports whether an event type ends an execution.
func IsTerminal(t string) bool {
	return t == runtime.EvExecSucceeded || t == runtime.EvExecFailed || t == runtime.EvExecCancelled
}
