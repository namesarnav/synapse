// Command wslat measures how long a persisted execution event takes to reach
// WebSocket subscribers. Latency is receive time minus the event's database
// created_at; both clocks are the same host, and created_at is the start of the
// writing transaction, so the figure slightly overstates the real delay.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5/pgxpool"
)

type msg struct {
	Type  string `json:"type"`
	Event *struct {
		ID        int64     `json:"id"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"event"`
}

type sample struct {
	typ string
	d   time.Duration
}

func post(h *http.Client, base, token, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %d %s", path, resp.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func pct(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[int(float64(len(s)-1)*p)]
}

func main() {
	api := flag.String("api", "http://127.0.0.1:18080", "API base URL")
	dbURL := flag.String("db", "", "Postgres URL of the stack under test")
	mode := flag.String("mode", "workspace", "workspace (K subscribers to the workspace stream) | execution (one socket per execution)")
	subs := flag.Int("subscribers", 1, "workspace subscribers")
	total := flag.Int("n", 300, "executions to run")
	rate := flag.Float64("rate", 20, "executions started per second")
	label := flag.String("label", "", "description of the stack")
	out := flag.String("out", "", "write the JSON result here")
	flag.Parse()
	ctx := context.Background()
	h := &http.Client{Timeout: 30 * time.Second}

	var sess struct {
		Token      string
		Workspaces []struct{ ID string } `json:"workspaces"`
	}
	if err := post(h, *api, "", "/api/v1/auth/register", map[string]any{"email": fmt.Sprintf("ws-%d@example.com", time.Now().UnixNano()), "password": "load-test-password-1", "display_name": "ws", "workspace_name": "ws"}, &sess); err != nil {
		fatal(err)
	}
	ws := sess.Workspaces[0].ID
	tf := func(id, e string) map[string]any {
		return map[string]any{"id": id, "type": "transform", "config": map[string]any{"output": map[string]any{"v": e}}}
	}
	g := map[string]any{
		"nodes": []any{map[string]any{"id": "t", "type": "manual_trigger", "config": map[string]any{}}, tf("a", "{{ trigger.i }}"), tf("b", "{{ nodes.a.v }}")},
		"edges": []any{map[string]any{"id": "e1", "source": "t", "target": "a"}, map[string]any{"id": "e2", "source": "a", "target": "b"}},
	}
	var wf struct{ ID string }
	if err := post(h, *api, sess.Token, "/api/v1/workspaces/"+ws+"/workflows", map[string]any{"name": "wslat", "graph": g}, &wf); err != nil {
		fatal(err)
	}
	if err := post(h, *api, sess.Token, "/api/v1/workspaces/"+ws+"/workflows/"+wf.ID+"/publish", nil, nil); err != nil {
		fatal(err)
	}
	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	wsBase := "ws" + strings.TrimPrefix(*api, "http")
	var mu sync.Mutex
	var samples []sample
	add := func(s []sample) { mu.Lock(); samples = append(samples, s...); mu.Unlock() }
	var wg sync.WaitGroup
	sctx, stop := context.WithCancel(ctx)

	if *mode == "workspace" {
		ready := make(chan struct{}, *subs)
		for i := 0; i < *subs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, _, err := websocket.Dial(sctx, wsBase+"/ws/workspaces/"+ws+"?token="+sess.Token, nil)
				if err != nil {
					fatal(err)
				}
				defer c.CloseNow()
				c.SetReadLimit(1 << 20)
				var local []sample
				first := true
				for {
					var m msg
					if err := wsjson.Read(sctx, c, &m); err != nil {
						break
					}
					if first {
						first = false
						ready <- struct{}{}
					}
					if m.Type == "event" && m.Event != nil {
						local = append(local, sample{m.Event.Type, time.Since(m.Event.CreatedAt)})
					}
				}
				add(local)
			}()
		}
		for i := 0; i < *subs; i++ {
			<-ready
		}
	}

	tk := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	defer tk.Stop()
	for i := 0; i < *total; i++ {
		<-tk.C
		var res struct{ Execution struct{ ID string } }
		if err := post(h, *api, sess.Token, "/api/v1/workspaces/"+ws+"/workflows/"+wf.ID+"/run", map[string]any{"trigger": map[string]any{"i": i}}, &res); err != nil {
			fatal(err)
		}
		if *mode == "execution" {
			connected := time.Now()
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				c, _, err := websocket.Dial(cctx, wsBase+"/ws/executions/"+id+"?token="+sess.Token, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				var local []sample
				for {
					var m msg
					if err := wsjson.Read(cctx, c, &m); err != nil {
						break
					}
					if m.Type == "event" && m.Event != nil {
						// events created before we connected are replays, not live pushes
						if m.Event.CreatedAt.After(connected) {
							local = append(local, sample{m.Event.Type, time.Since(m.Event.CreatedAt)})
						}
						if strings.HasPrefix(m.Event.Type, "execution.") && m.Event.Type != "execution.created" && m.Event.Type != "execution.started" {
							break
						}
					}
				}
				add(local)
			}(res.Execution.ID)
		}
	}

	var terminal int
	for dl := time.Now().Add(2 * time.Minute); time.Now().Before(dl); time.Sleep(200 * time.Millisecond) {
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE workflow_id=$1 AND status IN ('succeeded','failed','cancelled')`, wf.ID).Scan(&terminal)
		if terminal >= *total {
			break
		}
	}
	if *mode == "workspace" {
		time.Sleep(3 * time.Second) // let the last pushes arrive
		stop()
	}
	wg.Wait()
	stop()

	var expected int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM execution_events ev JOIN executions e ON e.id=ev.execution_id WHERE e.workflow_id=$1 AND ev.type LIKE 'execution.%' AND ev.type NOT IN ('execution.cancelling')`, wf.ID).Scan(&expected)
	if *mode == "workspace" {
		expected *= *subs
	}
	by := map[string][]float64{}
	var all []float64
	for _, s := range samples {
		ms := float64(s.d.Microseconds()) / 1000
		by[s.typ] = append(by[s.typ], ms)
		all = append(all, ms)
	}
	stat := func(v []float64) map[string]any {
		sort.Float64s(v)
		return map[string]any{"count": len(v), "p50_ms": pct(v, .5), "p95_ms": pct(v, .95), "p99_ms": pct(v, .99), "max_ms": pct(v, 1)}
	}
	types := map[string]any{}
	for t, v := range by {
		types[t] = stat(v)
	}
	res := map[string]any{
		"mode": *mode, "label": *label, "when": time.Now().UTC().Format(time.RFC3339), "subscribers": *subs, "executions": *total, "rate_per_s": *rate,
		"hardware":       map[string]any{"logical_cpus": runtime.NumCPU(), "os": runtime.GOOS + "/" + runtime.GOARCH, "go": runtime.Version()},
		"events_all":     stat(all),
		"events_by_type": types,
		"execution_level_events_expected_workspace_mode": expected,
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		_ = os.WriteFile(*out, append(b, '\n'), 0o644)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "wslat:", err)
	os.Exit(1)
}
