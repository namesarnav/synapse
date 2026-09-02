// Package localrun executes a workflow graph inside one process. It shares the
// engine's decision core with the durable runtime, so it doubles as an
// executable specification and a fast way to test graph semantics.
package localrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/nodes"
	"github.com/namesarnav/synapse/internal/workflow"
)

// Runner runs graphs in-process.
type Runner struct {
	Registry nodes.Registry
	Log      *slog.Logger
	Now      func() time.Time
	Secrets  func(name string) (string, bool)
	// Sleep waits for retries and delays; tests replace it to skip real time.
	Sleep func(ctx context.Context, d time.Duration) error
	// Workflows resolves sub-workflow ids to graphs.
	Workflows func(id string) (*workflow.Graph, error)
	MaxDepth  int
	// MaxParallel bounds concurrently running non-inline nodes (0 = unlimited).
	MaxParallel int
}

// TraceEvent records when a node started or finished, in coordinator order.
type TraceEvent struct {
	Seq   int
	Node  string
	Event string // start | end
}

// Result is the outcome of a run.
type Result struct {
	Status   engine.ExecState
	States   map[string]engine.NodeState
	Outputs  map[string]any
	Errors   map[string]*engine.NodeError
	Attempts map[string]int
	Output   any
	Error    string
	Trace    []TraceEvent
}

func (r *Runner) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.Sleep == nil {
		r.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if r.MaxDepth == 0 {
		r.MaxDepth = 5
	}
	if r.Registry == nil {
		r.Registry = nodes.NewRegistry(nodes.Options{})
	}
}

// Run executes g with the given trigger payload.
func (r *Runner) Run(ctx context.Context, g *workflow.Graph, trigger any) (*Result, error) {
	r.defaults()
	return r.run(ctx, g, runInput{trigger: trigger, depth: 0})
}

type runInput struct {
	trigger any
	start   string // explicit start node ("" = first trigger)
	item    any
	index   int
	inLoop  bool
	nodes   map[string]any // inherited outputs
	depth   int
}

type event struct {
	id      string
	kind    string // done | wake | retry | child
	out     any
	err     error
	attempt int
}

func (r *Runner) run(ctx context.Context, g *workflow.Graph, in runInput) (*Result, error) {
	g.Normalize()
	if rep := workflow.Validate(g, nil); !rep.Valid() && in.start == "" {
		return nil, fmt.Errorf("invalid workflow: %s", rep.Error())
	}
	ix := workflow.NewIndex(g)
	order, err := ix.TopoOrder()
	if err != nil {
		return nil, err
	}
	res := &Result{
		States: map[string]engine.NodeState{}, Outputs: map[string]any{}, Errors: map[string]*engine.NodeError{}, Attempts: map[string]int{},
	}
	st := engine.Statuses{}
	for _, n := range g.Nodes {
		st[n.ID] = &engine.Status{State: engine.NodePending, OnError: n.OnError}
	}
	c := &coord{r: r, ix: ix, order: order, st: st, res: res, in: in, events: make(chan event, 256)}
	c.ctx, c.cancel = context.WithCancel(ctx)
	defer c.cancel()
	if r.MaxParallel > 0 {
		c.sem = make(chan struct{}, r.MaxParallel)
	}
	c.ectx = &engine.Context{
		ExecutionID: "local", WorkflowID: "local", Trigger: in.trigger, Nodes: map[string]any{},
		Item: in.item, Index: in.index, InLoop: in.inLoop,
	}
	for k, v := range in.nodes {
		c.ectx.Nodes[k] = v
	}
	return c.loop()
}

type coord struct {
	r      *Runner
	ix     *workflow.Index
	order  []string
	st     engine.Statuses
	res    *Result
	in     runInput
	ctx    context.Context
	cancel context.CancelFunc
	ectx   *engine.Context
	events chan event
	sem    chan struct{}
	seq    int
	wg     sync.WaitGroup
	snapMu sync.Mutex
	stop   *engine.InlineResult
	failed string
}

func (c *coord) trace(node, ev string) {
	c.seq++
	c.res.Trace = append(c.res.Trace, TraceEvent{Seq: c.seq, Node: node, Event: ev})
}

func (c *coord) setState(id string, to engine.NodeState) {
	from := c.st[id].State
	if err := engine.CheckNodeTransition(from, to); err != nil {
		panic(fmt.Sprintf("%s: %v", id, err)) // engine bug: never reached by valid flows
	}
	c.st[id].State = to
}

func (c *coord) start() {
	starts := c.ix.Graph.Triggers()
	chosen := c.in.start
	if chosen == "" && len(starts) > 0 {
		chosen = starts[0].ID
	}
	for _, n := range c.ix.Graph.Nodes {
		if !n.Type.IsTrigger() {
			continue
		}
		if n.ID == chosen {
			c.setState(n.ID, engine.NodeReady)
		} else {
			c.setState(n.ID, engine.NodeSkipped)
		}
	}
}

func (c *coord) loop() (*Result, error) {
	c.start()
	for {
		c.settle()
		out := engine.Assess(c.st)
		if c.stop != nil || c.failed != "" || out.Done {
			break
		}
		select {
		case ev := <-c.events:
			c.handle(ev)
		case <-c.ctx.Done():
			c.finishCancelled()
			return c.finish(), nil
		}
	}
	c.cancel()
	c.wg.Wait()
	return c.finish(), nil
}

func (c *coord) finishCancelled() {
	for id, s := range c.st {
		if !s.State.Terminal() {
			if engine.CanNodeTransition(s.State, engine.NodeCancelled) {
				c.st[id].State = engine.NodeCancelled
			}
		}
	}
	c.res.Status = engine.ExecCancelled
	c.wg.Wait()
}

func (c *coord) finish() *Result {
	res := c.res
	for id, s := range c.st {
		if !s.State.Terminal() {
			if engine.CanNodeTransition(s.State, engine.NodeCancelled) {
				s.State = engine.NodeCancelled
			}
		}
		res.States[id] = s.State
	}
	if res.Status == "" {
		switch {
		case c.stop != nil && c.stop.StopStatus == "failed":
			res.Status = engine.ExecFailed
			res.Error = expressions.ToString(c.stop.Output.(map[string]any)["message"])
		case c.failed != "":
			res.Status = engine.ExecFailed
			res.Error = c.failed
		default:
			res.Status = engine.ExecSucceeded
		}
	}
	res.Output = engine.ResultOf(c.ix, c.order, c.st, res.Outputs)
	return res
}

// settle resolves skips and readiness and dispatches ready nodes until stable.
func (c *coord) settle() {
	for c.stop == nil && c.failed == "" {
		d := engine.Resolve(c.ix, c.order, c.st)
		if len(d.Skipped) == 0 && len(d.Ready) == 0 && !c.anyReady() {
			return
		}
		for _, id := range d.Skipped {
			c.setState(id, engine.NodeSkipped)
		}
		for _, id := range d.Ready {
			c.setState(id, engine.NodeReady)
		}
		for _, id := range c.order {
			if c.stop != nil || c.failed != "" {
				return
			}
			if c.st[id].State == engine.NodeReady {
				c.dispatch(id)
			}
		}
	}
}

func (c *coord) anyReady() bool {
	for _, s := range c.st {
		if s.State == engine.NodeReady {
			return true
		}
	}
	return false
}

func (c *coord) dispatch(id string) {
	n := c.ix.Nodes[id]
	if !n.Type.IsInline() {
		c.setState(id, engine.NodeQueued)
		c.launch(n, 1)
		return
	}
	var sources map[string]any
	if n.Type == workflow.TypeMerge {
		sources = map[string]any{}
		for _, s := range engine.ActiveSources(c.ix, id, c.st) {
			sources[s] = c.res.Outputs[s]
		}
	}
	res := engine.EvalInline(n, c.ectx, c.ectx.Env(c.r.Secrets, nil, c.r.Now), c.r.Now(), sources)
	c.apply(n, res)
}

func (c *coord) apply(n *workflow.Node, res engine.InlineResult) {
	id := n.ID
	switch res.Kind {
	case engine.InlineDone:
		c.trace(id, "start")
		c.complete(id, res.Output, res.Branch)
	case engine.InlineFail:
		c.trace(id, "start")
		c.failNode(n, res.Err, 1)
	case engine.InlineStop:
		c.trace(id, "start")
		c.res.Outputs[id] = res.Output
		c.setState(id, engine.NodeSucceeded)
		c.trace(id, "end")
		r := res
		c.stop = &r
	case engine.InlineWait:
		c.trace(id, "start")
		c.setState(id, engine.NodeWaiting)
		c.res.Outputs[id] = res.Output
		wait := time.Until(res.WakeAt)
		c.spawn(func() {
			if c.r.Sleep(c.ctx, wait) == nil {
				c.events <- event{id: id, kind: "wake"}
			}
		})
	case engine.InlineForEach:
		c.trace(id, "start")
		c.setState(id, engine.NodeWaiting)
		c.spawn(func() { c.runForEach(n, res) })
	case engine.InlineSub:
		c.trace(id, "start")
		c.setState(id, engine.NodeWaiting)
		c.spawn(func() { c.runSub(n, res) })
	}
}

func (c *coord) spawn(f func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		f()
	}()
}

func (c *coord) send(ev event) {
	select {
	case c.events <- ev:
	case <-c.ctx.Done():
	}
}

func (c *coord) complete(id string, out any, branch string) {
	if _, ok := c.res.Outputs[id]; !ok || out != nil {
		c.res.Outputs[id] = out
	}
	c.publish(id)
	c.st[id].Branch = branch
	c.setState(id, engine.NodeSucceeded)
	c.trace(id, "end")
}

// publish exposes a node's output to expressions.
func (c *coord) publish(id string) {
	c.snapMu.Lock()
	c.ectx.Nodes[id] = c.res.Outputs[id]
	c.snapMu.Unlock()
}

func (c *coord) failNode(n *workflow.Node, err *engine.NodeError, attempt int) {
	c.res.Errors[n.ID] = err
	c.res.Attempts[n.ID] = attempt
	if n.OnError == workflow.OnErrorContinue {
		c.res.Outputs[n.ID] = map[string]any{"error": err.Message, "code": err.Code}
		c.publish(n.ID)
		c.st[n.ID].Branch = engine.ContinueBranch(n.Type)
		c.setState(n.ID, engine.NodeFailed)
		c.trace(n.ID, "end")
		return
	}
	c.setState(n.ID, engine.NodeFailed)
	c.trace(n.ID, "end")
	c.failed = fmt.Sprintf("node %s failed: %s", n.ID, err.Message)
}

func (c *coord) launch(n *workflow.Node, attempt int) {
	c.spawn(func() {
		if c.sem != nil {
			select {
			case c.sem <- struct{}{}:
				defer func() { <-c.sem }()
			case <-c.ctx.Done():
				return
			}
		}
		c.send(event{id: n.ID, kind: "start", attempt: attempt})
		out, err := c.exec(n, attempt)
		c.send(event{id: n.ID, kind: "done", out: out, err: err, attempt: attempt})
	})
}

func (c *coord) exec(n *workflow.Node, attempt int) (any, error) {
	ex, ok := c.r.Registry[n.Type]
	if !ok {
		return nil, &engine.NodeError{Code: engine.CodeInternal, Message: "no executor for " + string(n.Type)}
	}
	cfg, err := workflow.DecodeConfig(n)
	if err != nil {
		return nil, &engine.NodeError{Code: engine.CodeConfig, Message: err.Error()}
	}
	// Executors see a snapshot; c.ectx is owned by the coordinator goroutine.
	c.snapMu.Lock()
	snap := *c.ectx
	snap.Nodes = make(map[string]any, len(c.ectx.Nodes))
	for k, v := range c.ectx.Nodes {
		snap.Nodes[k] = v
	}
	c.snapMu.Unlock()
	return ex.Execute(c.ctx, nodes.Input{
		Node: n, Config: cfg, Env: snap.Env(c.r.Secrets, nil, c.r.Now), Attempt: attempt,
		IdempotencyKey: fmt.Sprintf("local:%s:%d", n.ID, attempt), Log: c.r.Log,
	})
}

func (c *coord) handle(ev event) {
	n := c.ix.Nodes[ev.id]
	st := c.st[ev.id]
	switch ev.kind {
	case "start":
		if st.State == engine.NodeQueued || st.State == engine.NodeRetrying {
			c.setState(ev.id, engine.NodeRunning)
			c.res.Attempts[ev.id] = ev.attempt
			c.trace(ev.id, "start")
		}
	case "done":
		if st.State != engine.NodeRunning {
			return
		}
		if ev.err == nil {
			c.complete(ev.id, ev.out, "")
			c.res.Attempts[ev.id] = ev.attempt
			return
		}
		ne := asNodeError(ev.err)
		if ev.out != nil {
			c.res.Outputs[ev.id] = ev.out // keep partial output (e.g. HTTP response) for debugging
		}
		if ne.Retryable && n.Retry.CanRetry(ev.attempt) {
			c.setState(ev.id, engine.NodeRetrying)
			delay := n.Retry.Delay(ev.attempt, rand.Float64)
			next := ev.attempt + 1
			c.spawn(func() {
				if c.r.Sleep(c.ctx, delay) == nil {
					c.send(event{id: ev.id, kind: "retry", attempt: next})
				}
			})
			return
		}
		c.failNode(n, ne, ev.attempt)
	case "retry":
		if st.State == engine.NodeRetrying {
			c.launch(n, ev.attempt)
		}
	case "wake":
		if st.State == engine.NodeWaiting {
			c.complete(ev.id, c.res.Outputs[ev.id], "")
		}
	case "child":
		if st.State != engine.NodeWaiting {
			return
		}
		if ev.err != nil {
			c.failNode(n, asNodeError(ev.err), 1)
			return
		}
		branch := ""
		if n.Type == workflow.TypeForEach {
			branch = workflow.BranchDone
		}
		c.complete(ev.id, ev.out, branch)
	}
}

func asNodeError(err error) *engine.NodeError {
	var ne *engine.NodeError
	if errors.As(err, &ne) {
		return ne
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &engine.NodeError{Code: engine.CodeTimeout, Message: err.Error(), Retryable: true}
	}
	return &engine.NodeError{Code: engine.CodeInternal, Message: err.Error(), Retryable: true}
}
