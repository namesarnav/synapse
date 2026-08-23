// Command loadgen drives a running Synapse stack and records end-to-end
// numbers. It submits executions over the public API, then reads server-side
// timestamps from Postgres so latency excludes client polling error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type client struct {
	base, token string
	h           *http.Client
}

func (c *client) do(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

type node = map[string]any

func graph(scenario, echoURL string) map[string]any {
	trig := node{"id": "t", "type": "manual_trigger", "config": node{}}
	tf := func(id, expr string) node {
		return node{"id": id, "type": "transform", "config": node{"output": node{"v": expr}}}
	}
	edge := func(a, b string) node { return node{"id": a + "_" + b, "source": a, "target": b} }
	switch scenario {
	case "chain":
		return map[string]any{
			"nodes": []node{trig, tf("a", "{{ trigger.i }}"), tf("b", "{{ nodes.a.v }}"), tf("c", "{{ nodes.b.v }}")},
			"edges": []node{edge("t", "a"), edge("a", "b"), edge("b", "c")},
		}
	case "fanout":
		nodes := []node{trig}
		var edges []node
		for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
			nodes = append(nodes, tf(id, "{{ trigger.i }}"))
			edges = append(edges, edge("t", id), edge(id, "m"))
		}
		nodes = append(nodes, node{"id": "m", "type": "merge", "config": node{}})
		return map[string]any{"nodes": nodes, "edges": edges}
	case "http":
		return map[string]any{
			"nodes": []node{trig, {"id": "h", "type": "http_request", "config": node{"method": "GET", "url": echoURL}}},
			"edges": []node{edge("t", "h")},
		}
	}
	fmt.Fprintln(os.Stderr, "unknown scenario", scenario)
	os.Exit(2)
	return nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func cpuModel() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "model name") {
			return strings.TrimSpace(l[strings.Index(l, ":")+1:])
		}
	}
	return "unknown"
}

func memGB() float64 {
	b, _ := os.ReadFile("/proc/meminfo")
	var kb float64
	fmt.Sscanf(strings.SplitN(string(b), "\n", 2)[0], "MemTotal: %f kB", &kb)
	return kb / 1024 / 1024
}

func gitRev() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func main() {
	api := flag.String("api", "http://127.0.0.1:18080", "API base URL")
	dbURL := flag.String("db", "postgres://synapse:synapse@localhost:55432/synapse_load?sslmode=disable", "Postgres URL of the stack under test")
	scenario := flag.String("scenario", "chain", "chain | fanout | http")
	total := flag.Int("n", 2000, "executions to submit")
	conc := flag.Int("c", 32, "concurrent submitters")
	rate := flag.Float64("rate", 0, "submissions per second across all submitters (0 = as fast as possible)")
	echoAddr := flag.String("echo", "127.0.0.1:18099", "address of the in-process echo server (http scenario)")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up waiting for executions to finish")
	label := flag.String("label", "", "free-text description of the stack (workers, capacity, postgres settings)")
	out := flag.String("out", "", "write the JSON result here")
	flag.Parse()
	ctx := context.Background()

	if *scenario == "http" {
		ln, err := net.Listen("tcp", *echoAddr)
		if err != nil {
			fatal(err)
		}
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	}

	c := &client{base: *api, h: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *conc * 2}}}
	email := fmt.Sprintf("load-%d@example.com", time.Now().UnixNano())
	var sess struct {
		Token      string
		Workspaces []struct{ ID string } `json:"workspaces"`
	}
	if _, err := c.do("POST", "/api/v1/auth/register", map[string]any{"email": email, "password": "load-test-password-1", "display_name": "load", "workspace_name": "load"}, &sess); err != nil {
		fatal(err)
	}
	c.token = sess.Token
	ws := sess.Workspaces[0].ID
	var wf struct{ ID string }
	if _, err := c.do("POST", "/api/v1/workspaces/"+ws+"/workflows", map[string]any{"name": *scenario, "graph": graph(*scenario, "http://"+*echoAddr+"/")}, &wf); err != nil {
		fatal(err)
	}
	wid := wf.ID
	if _, err := c.do("POST", "/api/v1/workspaces/"+ws+"/workflows/"+wid+"/publish", nil, nil); err != nil {
		fatal(err)
	}

	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	// Submit.
	jobs := make(chan int)
	var mu sync.Mutex
	var lat []float64
	var errs int
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				s := time.Now()
				_, err := c.do("POST", "/api/v1/workspaces/"+ws+"/workflows/"+wid+"/run", map[string]any{"trigger": map[string]any{"i": j}}, nil)
				d := time.Since(s).Seconds()
				mu.Lock()
				if err != nil {
					errs++
				} else {
					lat = append(lat, d)
				}
				mu.Unlock()
			}
		}()
	}
	var tick <-chan time.Time
	if *rate > 0 {
		tk := time.NewTicker(time.Duration(float64(time.Second) / *rate))
		defer tk.Stop()
		tick = tk.C
	}
	for i := 0; i < *total; i++ {
		if tick != nil {
			<-tick
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	submitDur := time.Since(t0)

	// Wait for every accepted execution to finish.
	accepted := len(lat)
	deadline := time.Now().Add(*timeout)
	var terminal int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE workflow_id=$1 AND status IN ('succeeded','failed','cancelled')`, wid).Scan(&terminal); err != nil {
			fatal(err)
		}
		if terminal >= accepted {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	var ok, failed int
	var first, last time.Time
	var p50, p95, p99, maxE, meanE float64
	err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='succeeded'), count(*) FILTER (WHERE status='failed'),
		coalesce(min(created_at), now()), coalesce(max(finished_at), now())
		FROM executions WHERE workflow_id=$1`, wid).Scan(&ok, &failed, &first, &last)
	if err != nil {
		fatal(err)
	}
	err = pool.QueryRow(ctx, `SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY d),0), coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY d),0),
		coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY d),0), coalesce(max(d),0), coalesce(avg(d),0)
		FROM (SELECT extract(epoch FROM finished_at - created_at) d FROM executions WHERE workflow_id=$1 AND status='succeeded') x`, wid).Scan(&p50, &p95, &p99, &maxE, &meanE)
	if err != nil {
		fatal(err)
	}
	var tasks, redelivered int
	_ = pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE delivery_count > 1) FROM tasks t JOIN executions e ON e.id=t.execution_id WHERE e.workflow_id=$1`, wid).Scan(&tasks, &redelivered)

	sort.Float64s(lat)
	drain := last.Sub(first).Seconds()
	res := map[string]any{
		"scenario": *scenario, "label": *label, "when": time.Now().UTC().Format(time.RFC3339), "git": gitRev(),
		"hardware":  map[string]any{"cpu": cpuModel(), "logical_cpus": runtime.NumCPU(), "memory_gb": fmt.Sprintf("%.1f", memGB()), "os": runtime.GOOS + "/" + runtime.GOARCH, "go": runtime.Version()},
		"submitted": *total, "accepted": accepted, "submit_errors": errs, "submit_concurrency": *conc, "target_rate_per_s": *rate,
		"succeeded": ok, "failed": failed, "unfinished": accepted - terminal, "tasks": tasks, "redelivered_tasks": redelivered,
		"submit_seconds": submitDur.Seconds(), "submit_per_s": float64(accepted) / submitDur.Seconds(),
		"submit_latency_ms": map[string]float64{"p50": pct(lat, .5) * 1000, "p95": pct(lat, .95) * 1000, "p99": pct(lat, .99) * 1000},
		"end_to_end_ms":     map[string]float64{"p50": p50 * 1000, "p95": p95 * 1000, "p99": p99 * 1000, "max": maxE * 1000, "mean": meanE * 1000},
		"drain_seconds":     drain, "completed_per_s": float64(ok) / drain, "tasks_per_s": float64(tasks) / drain,
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			fatal(err)
		}
	}
	if failed > 0 || accepted-terminal > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "loadgen:", err)
	os.Exit(1)
}
