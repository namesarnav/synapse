// Package protocol checks that the TypeScript contracts in packages/protocol
// list exactly the values the Go backend defines.
package protocol

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/workflow"
)

const tsFile = "../../packages/protocol/src/index.ts"

// tsList extracts the string members of `export const NAME = [ ... ] as const`.
func tsList(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)export const ` + name + ` = \[(.*?)\] as const`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s not found in %s", name, tsFile)
	}
	var out []string
	for _, s := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, s[1])
	}
	return out
}

func same(t *testing.T, name string, ts, backend []string) {
	t.Helper()
	a, b := append([]string(nil), ts...), append([]string(nil), backend...)
	sort.Strings(a)
	sort.Strings(b)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("%s drifted\n  ts: %v\n  go: %v", name, a, b)
	}
}

func TestProtocolMatchesBackend(t *testing.T) {
	raw, err := os.ReadFile(tsFile)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	var nodeTypes []string
	for _, ti := range workflow.Catalog() {
		nodeTypes = append(nodeTypes, string(ti.Type))
	}
	same(t, "NODE_TYPES", tsList(t, src, "NODE_TYPES"), nodeTypes)

	var nodeStates, execStates []string
	for _, s := range engine.AllNodeStates {
		nodeStates = append(nodeStates, string(s))
	}
	for _, s := range engine.AllExecStates {
		execStates = append(execStates, string(s))
	}
	same(t, "NODE_STATES", tsList(t, src, "NODE_STATES"), nodeStates)
	same(t, "EXEC_STATES", tsList(t, src, "EXEC_STATES"), execStates)
	same(t, "EVENT_TYPES", tsList(t, src, "EVENT_TYPES"), runtime.AllEventTypes)
	same(t, "MESSAGE_TYPES", tsList(t, src, "MESSAGE_TYPES"), realtime.MessageTypes)
	same(t, "ROLES", tsList(t, src, "ROLES"),
		[]string{string(auth.RoleViewer), string(auth.RoleMember), string(auth.RoleAdmin), string(auth.RoleOwner)})
}

func TestProtocolVersionIsDeclared(t *testing.T) {
	raw, err := os.ReadFile(tsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`export const PROTOCOL_VERSION = \d+;`).Match(raw) {
		t.Fatal("PROTOCOL_VERSION missing")
	}
}
