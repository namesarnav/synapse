// Package telemetry defines the Prometheus metrics for every Synapse process.
// Each Metrics owns a private registry so tests and processes never collide.
package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/namesarnav/synapse/internal/persistence"
	"github.com/namesarnav/synapse/internal/realtime"
	"github.com/namesarnav/synapse/internal/runtime"
	"github.com/namesarnav/synapse/internal/scheduler"
	"github.com/namesarnav/synapse/internal/worker"
)

var (
	latency = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}
	// long-running node and execution durations
	longRun = []float64{.01, .05, .1, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600}
)

// Metrics holds every collector.
type Metrics struct {
	Reg *prometheus.Registry

	httpReqs *prometheus.CounterVec
	httpDur  *prometheus.HistogramVec

	events        *prometheus.CounterVec
	execFinished  *prometheus.CounterVec
	execDuration  *prometheus.HistogramVec
	nodeRetries   prometheus.Counter
	tasksDone     *prometheus.CounterVec
	taskDuration  *prometheus.HistogramVec
	tasksClaimed  prometheus.Counter
	leasesLost    prometheus.Counter
	wsClients     prometheus.Gauge
	wsDropped     prometheus.Counter
	schedulerWork *prometheus.CounterVec
}

// New builds the collectors and registers them, plus Go and process metrics.
func New() *Metrics {
	m := &Metrics{Reg: prometheus.NewRegistry()}
	f := func(c prometheus.Collector) { m.Reg.MustRegister(c) }
	m.httpReqs = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "synapse_http_requests_total", Help: "HTTP requests by method, route and status class."}, []string{"method", "route", "status"})
	m.httpDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "synapse_http_request_duration_seconds", Help: "HTTP request latency.", Buckets: latency}, []string{"method", "route"})
	m.events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "synapse_events_total", Help: "Execution events emitted, by type."}, []string{"type"})
	m.execFinished = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "synapse_executions_finished_total", Help: "Executions reaching a terminal state."}, []string{"status"})
	m.execDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "synapse_execution_duration_seconds", Help: "Time from creation to a terminal state.", Buckets: longRun}, []string{"status"})
	m.nodeRetries = prometheus.NewCounter(prometheus.CounterOpts{Name: "synapse_node_retries_total", Help: "Node attempts scheduled for retry."})
	m.tasksDone = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "synapse_worker_tasks_total", Help: "Tasks finished by workers, by node type and outcome."}, []string{"node_type", "outcome"})
	m.taskDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "synapse_worker_task_duration_seconds", Help: "Node execution time.", Buckets: longRun}, []string{"node_type"})
	m.tasksClaimed = prometheus.NewCounter(prometheus.CounterOpts{Name: "synapse_worker_tasks_claimed_total", Help: "Tasks claimed from the queue."})
	m.leasesLost = prometheus.NewCounter(prometheus.CounterOpts{Name: "synapse_worker_leases_lost_total", Help: "Results dropped because the lease was lost."})
	m.wsClients = prometheus.NewGauge(prometheus.GaugeOpts{Name: "synapse_ws_clients", Help: "Connected WebSocket clients."})
	m.wsDropped = prometheus.NewCounter(prometheus.CounterOpts{Name: "synapse_ws_slow_clients_dropped_total", Help: "Clients disconnected for not keeping up."})
	m.schedulerWork = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "synapse_scheduler_actions_total", Help: "Scheduler actions taken, by kind."}, []string{"action"})
	for _, c := range []prometheus.Collector{m.httpReqs, m.httpDur, m.events, m.execFinished, m.execDuration, m.nodeRetries, m.tasksDone,
		m.taskDuration, m.tasksClaimed, m.leasesLost, m.wsClients, m.wsDropped, m.schedulerWork,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})} {
		f(c)
	}
	return m
}

// Handler serves the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}

// ObserveHTTP records one served request.
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	m.httpReqs.WithLabelValues(method, route, strconv.Itoa(status/100)+"xx").Inc()
	m.httpDur.WithLabelValues(method, route).Observe(d.Seconds())
}

// ObserveEvents counts emitted events and derives execution outcome metrics.
func (m *Metrics) ObserveEvents(evs []runtime.Event) {
	for _, e := range evs {
		m.events.WithLabelValues(e.Type).Inc()
		switch e.Type {
		case runtime.EvNodeRetrying:
			m.nodeRetries.Inc()
		case runtime.EvExecSucceeded, runtime.EvExecFailed, runtime.EvExecCancelled:
			status := e.Type[len("execution."):]
			m.execFinished.WithLabelValues(status).Inc()
			if ms, ok := durationMS(e.Data["duration_ms"]); ok {
				m.execDuration.WithLabelValues(status).Observe(ms / 1000)
			}
		}
	}
}

// Worker returns hooks for a worker.
func (m *Metrics) Worker() worker.Metrics {
	return worker.Metrics{
		TaskDone: func(nodeType, outcome string, d time.Duration) {
			m.tasksDone.WithLabelValues(nodeType, outcome).Inc()
			m.taskDuration.WithLabelValues(nodeType).Observe(d.Seconds())
		},
		Claimed: func(n int) { m.tasksClaimed.Add(float64(n)) },
		Lost:    m.leasesLost.Inc,
	}
}

// Scheduler returns hooks for the scheduler loops.
func (m *Metrics) Scheduler() scheduler.Metrics {
	add := func(action string) func(int) {
		return func(n int) { m.schedulerWork.WithLabelValues(action).Add(float64(n)) }
	}
	return scheduler.Metrics{Woken: add("woken"), Expired: add("lease_expired"), Requeued: add("requeued"),
		TimedOut: add("deadline_timeout"), Swept: add("parent_swept")}
}

// Hub returns hooks for the WebSocket hub.
func (m *Metrics) Hub() realtime.Metrics {
	return realtime.Metrics{Dropped: m.wsDropped.Inc, Clients: func(d int) { m.wsClients.Add(float64(d)) }}
}

// RegisterDB adds gauges sampled from Postgres at scrape time: queue depth,
// oldest queued task age, workers and executions by status.
func (m *Metrics) RegisterDB(db *persistence.DB) {
	m.Reg.MustRegister(&dbCollector{db: db,
		queue:   prometheus.NewDesc("synapse_queue_tasks", "Tasks by status.", []string{"status"}, nil),
		oldest:  prometheus.NewDesc("synapse_queue_oldest_ready_age_seconds", "Age of the oldest task that is ready to run.", nil, nil),
		workers: prometheus.NewDesc("synapse_workers", "Registered workers by status.", []string{"status"}, nil),
		execs:   prometheus.NewDesc("synapse_executions", "Executions by status.", []string{"status"}, nil),
	})
}

type dbCollector struct {
	db                            *persistence.DB
	queue, oldest, workers, execs *prometheus.Desc
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queue
	ch <- c.oldest
	ch <- c.workers
	ch <- c.execs
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	group := func(desc *prometheus.Desc, sql string, known ...string) {
		counts := map[string]float64{}
		for _, k := range known {
			counts[k] = 0 // keep a series for every state so dashboards never see gaps
		}
		rows, err := c.db.Pool.Query(ctx, sql)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var n int64
			if rows.Scan(&st, &n) == nil {
				counts[st] = float64(n)
			}
		}
		for st, n := range counts {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, n, st)
		}
	}
	group(c.queue, `SELECT status, count(*) FROM tasks WHERE status IN ('queued','leased','dead') GROUP BY status`, "queued", "leased", "dead")
	group(c.workers, `SELECT status, count(*) FROM workers GROUP BY status`, "active", "draining", "stopped", "dead")
	group(c.execs, `SELECT status, count(*) FROM executions GROUP BY status`,
		"created", "running", "waiting", "succeeded", "failed", "cancelling", "cancelled")
	var age float64
	if err := c.db.Pool.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - min(run_at)), 0) FROM tasks WHERE status='queued' AND run_at <= now()`).Scan(&age); err == nil {
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, age)
	}
}

func durationMS(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
