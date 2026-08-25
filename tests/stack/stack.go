// Package stack starts the real Synapse binaries against a throwaway database
// so integration and end-to-end tests can kill, restart and observe them.
package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// moduleRoot walks up from the working directory to go.mod.
func moduleRoot() string {
	dir, _ := os.Getwd()
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return dir
}

// Build compiles the api and worker once per test binary and returns the directory.
// SYNAPSE_STACK_RACE=1 builds them with the race detector.
func Build(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "synapse-stack-*")
		if buildErr != nil {
			return
		}
		for _, app := range []string{"api", "worker"} {
			args := []string{"build"}
			if os.Getenv("SYNAPSE_STACK_RACE") == "1" { // race-instrument the service binaries
				args = append(args, "-race")
			}
			cmd := exec.Command("go", append(args, "-o", filepath.Join(binDir, app), "./apps/"+app)...)
			cmd.Dir = moduleRoot()
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", app, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binDir
}

// FreeAddr returns a loopback address with a currently free port.
func FreeAddr(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// Proc is a running service process.
type Proc struct {
	Name string
	Addr string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	out  *safeBuf
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Logs returns everything the process has written so far.
func (p *Proc) Logs() string { return p.out.String() }

// Kill sends SIGKILL and waits for the process to disappear (a crash: no cleanup runs).
func (p *Proc) Kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.done
}

// Stop sends SIGTERM and waits up to timeout; it reports the exit error, if any.
func (p *Proc) Stop(timeout time.Duration) error {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return p.err
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("%s did not exit within %s", p.Name, timeout)
	}
}

// Running reports whether the process is still alive.
func (p *Proc) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Stack owns the processes of one test.
type Stack struct {
	T      testing.TB
	DBURL  string
	APIURL string
	Extra  []string // extra KEY=VALUE environment for every process
	bin    string
	apiAdr string
	api    *Proc
	procs  []*Proc
}

// New prepares a stack over the given (already migrated or empty) database.
func New(t testing.TB, dbURL string, extraEnv ...string) *Stack {
	t.Helper()
	s := &Stack{T: t, DBURL: dbURL, bin: Build(t), Extra: extraEnv}
	t.Cleanup(s.Close)
	return s
}

func (s *Stack) env(more ...string) []string {
	e := append(os.Environ(),
		"SYNAPSE_DATABASE_URL="+s.DBURL,
		"SYNAPSE_LOG_LEVEL=info", "SYNAPSE_REDIS_URL=",
		"SYNAPSE_HTTP_ALLOW_PRIVATE=true",
		"SYNAPSE_AUTH_RATE_PER_MIN=100000", "SYNAPSE_API_RATE=100000", "SYNAPSE_API_BURST=100000",
		"SYNAPSE_LEASE_DURATION=2s", "SYNAPSE_HEARTBEAT_INTERVAL=400ms", "SYNAPSE_WORKER_DEAD_AFTER=3s",
		"SYNAPSE_SCHEDULER_TICK=200ms", "SYNAPSE_POLL_INTERVAL=100ms", "SYNAPSE_SHUTDOWN_TIMEOUT=5s",
		"SYNAPSE_SWEEP_AFTER=2s",
		"SYNAPSE_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	e = append(e, s.Extra...)
	return append(e, more...)
}

func (s *Stack) start(name, bin string, addr string, env []string) *Proc {
	s.T.Helper()
	cmd := exec.Command(filepath.Join(s.bin, bin))
	cmd.Env = env
	out := &safeBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		s.T.Fatalf("start %s: %v", name, err)
	}
	p := &Proc{Name: name, Addr: addr, cmd: cmd, done: make(chan struct{}), out: out}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	s.procs = append(s.procs, p)
	return p
}

// StartAPI runs the API (with the in-process scheduler). A restart reuses the same address.
func (s *Stack) StartAPI(more ...string) *Proc {
	s.T.Helper()
	if s.apiAdr == "" {
		s.apiAdr = FreeAddr(s.T)
		s.APIURL = "http://" + s.apiAdr
	}
	p := s.start("api", "api", s.apiAdr, s.env(append([]string{"SYNAPSE_HTTP_ADDR=" + s.apiAdr}, more...)...))
	s.api = p
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !p.Running() {
			s.T.Fatalf("api exited early:\n%s", p.Logs())
		}
		if r, err := http.Get(s.APIURL + "/health/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.T.Fatalf("api not ready:\n%s", p.Logs())
	return nil
}

// API returns the most recently started API process.
func (s *Stack) API() *Proc { return s.api }

// StartWorker runs a worker with the given id and capacity.
func (s *Stack) StartWorker(id string, capacity int, more ...string) *Proc {
	s.T.Helper()
	addr := FreeAddr(s.T)
	return s.start("worker "+id, "worker", addr, s.env(append([]string{
		"SYNAPSE_WORKER_ID=" + id, fmt.Sprintf("SYNAPSE_WORKER_CAPACITY=%d", capacity), "SYNAPSE_WORKER_HTTP_ADDR=" + addr}, more...)...))
}

// Close kills whatever is still running and dumps logs of failed tests.
func (s *Stack) Close() {
	for _, p := range s.procs {
		if p.Running() {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if strings.Contains(p.Logs(), "WARNING: DATA RACE") {
			s.T.Errorf("%s reported a data race:\n%s", p.Name, tail(p.Logs(), 6000))
		}
		if s.T.Failed() {
			s.T.Logf("--- %s logs ---\n%s", p.Name, tail(p.Logs(), 4000))
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// Client is a bearer-token JSON client for the API.
type Client struct {
	T           testing.TB
	Base        string
	Token       string
	WorkspaceID string
	HTTP        *http.Client
}

// NewClient registers a fresh user and returns a client for its workspace.
func (s *Stack) NewClient(email string) *Client {
	s.T.Helper()
	c := &Client{T: s.T, Base: s.APIURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
	var r struct {
		Token      string
		Workspaces []struct{ ID string }
	}
	c.MustDo("POST", "/api/v1/auth/register", map[string]any{"email": email, "password": "correct-horse-battery", "display_name": "T"}, &r)
	c.Token, c.WorkspaceID = r.Token, r.Workspaces[0].ID
	return c
}

// Do performs a request and returns the status; a non-nil out receives the JSON body.
func (c *Client) Do(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.Base+path, rd)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil && len(data) > 0 && res.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, fmt.Errorf("decode %q: %w", data, err)
		}
	}
	if res.StatusCode >= 400 {
		return res.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(data)))
	}
	return res.StatusCode, nil
}

// MustDo is Do that fails the test on error.
func (c *Client) MustDo(method, path string, body, out any) {
	c.T.Helper()
	if _, err := c.Do(method, path, body, out); err != nil {
		c.T.Fatal(err)
	}
}

// Publish creates and publishes a workflow, returning its id.
func (c *Client) Publish(name string, graph map[string]any) string {
	c.T.Helper()
	var wf struct{ ID string }
	base := "/api/v1/workspaces/" + c.WorkspaceID + "/workflows"
	c.MustDo("POST", base, map[string]any{"name": name, "graph": graph}, &wf)
	c.MustDo("POST", base+"/"+wf.ID+"/publish", nil, nil)
	return wf.ID
}

// Run starts a manual execution and returns its id.
func (c *Client) Run(workflowID string, trigger any) string {
	c.T.Helper()
	var r struct{ Execution struct{ ID string } }
	c.MustDo("POST", "/api/v1/workspaces/"+c.WorkspaceID+"/workflows/"+workflowID+"/run", map[string]any{"trigger": trigger}, &r)
	return r.Execution.ID
}

// Execution is the subset of an execution the tests look at.
type Execution struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Get fetches an execution.
func (c *Client) Get(id string) (Execution, error) {
	var r struct{ Execution Execution }
	_, err := c.Do("GET", "/api/v1/workspaces/"+c.WorkspaceID+"/executions/"+id, nil, &r)
	return r.Execution, err
}

// IsTerminal reports a finished execution status.
func IsTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}

// Wait polls until the execution is terminal (API errors during outages are retried).
func (c *Client) Wait(id string, timeout time.Duration) Execution {
	c.T.Helper()
	deadline := time.Now().Add(timeout)
	var last Execution
	for time.Now().Before(deadline) {
		if ex, err := c.Get(id); err == nil {
			last = ex
			if IsTerminal(ex.Status) {
				return ex
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.T.Fatalf("execution %s not finished after %s (last status %q)", id, timeout, last.Status)
	return last
}

// Event mirrors the API event shape.
type Event struct {
	ID     int64          `json:"id"`
	Type   string         `json:"type"`
	NodeID string         `json:"node_id"`
	Data   map[string]any `json:"data"`
}

// Events returns the execution's event log after the given id.
func (c *Client) Events(id string, after int64) []Event {
	c.T.Helper()
	var r struct{ Events []Event }
	c.MustDo("GET", fmt.Sprintf("/api/v1/workspaces/%s/executions/%s/events?after=%d&limit=1000", c.WorkspaceID, id, after), nil, &r)
	return r.Events
}

// Ctx returns a context cancelled with the test.
func Ctx(t testing.TB, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
