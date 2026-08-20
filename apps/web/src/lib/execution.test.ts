import { describe, expect, it } from "vitest";
import type { ExecEvent, ExecutionDetail } from "@protocol";
import { applyEvent, buildTimeline, durationMs, emptyView, formatDuration, fromDetail } from "./execution";

let seq = 0;
const ev = (type: ExecEvent["type"], node?: string, extra: Partial<ExecEvent> = {}): ExecEvent => ({
  id: ++seq,
  execution_id: "x",
  type,
  node_id: node,
  created_at: new Date(1_700_000_000_000 + seq * 100).toISOString(),
  ...extra,
});

describe("applyEvent", () => {
  it("tracks a node through its lifecycle", () => {
    let v = emptyView();
    v = applyEvent(v, ev("execution.started"));
    v = applyEvent(v, ev("node.queued", "a"));
    expect(v.nodes.a.state).toBe("queued");
    v = applyEvent(v, ev("node.started", "a", { attempt: 1, data: { worker: "w1" } }));
    expect(v.nodes.a).toMatchObject({ state: "running", worker: "w1", attempt: 1 });
    v = applyEvent(v, ev("node.succeeded", "a"));
    expect(v.nodes.a.state).toBe("succeeded");
    expect(v.status).toBe("running");
    v = applyEvent(v, ev("execution.succeeded"));
    expect(v.status).toBe("succeeded");
  });

  it("counts retries and records the error", () => {
    let v = emptyView();
    v = applyEvent(v, ev("node.started", "a", { attempt: 1 }));
    v = applyEvent(v, ev("node.retrying", "a", { data: { error: { code: "http_error", message: "503" }, delay_ms: 1000 } }));
    expect(v.nodes.a).toMatchObject({ state: "retrying", retries: 1 });
    expect(v.nodes.a.error?.code).toBe("http_error");
    v = applyEvent(v, ev("node.started", "a", { attempt: 2 }));
    expect(v.nodes.a.error).toBeUndefined();
    expect(v.nodes.a.retries).toBe(1);
  });

  it("ignores events it has already applied", () => {
    const e = ev("node.started", "a");
    const once = applyEvent(emptyView(), e);
    const twice = applyEvent(once, e);
    expect(twice).toBe(once);
    expect(twice.events).toHaveLength(1);
  });

  it("ignores stale events that arrive out of order", () => {
    const first = ev("node.started", "a");
    const second = ev("node.succeeded", "a");
    let v = applyEvent(emptyView(), second);
    v = applyEvent(v, first);
    expect(v.nodes.a.state).toBe("succeeded");
  });

  it("records the wake time of a durable delay", () => {
    const v = applyEvent(emptyView(), ev("node.waiting", "d", { data: { wake_at: "2030-01-01T00:00:00Z" } }));
    expect(v.nodes.d).toMatchObject({ state: "waiting", wakeAt: "2030-01-01T00:00:00Z" });
  });

  it("moves execution status on cancellation events", () => {
    let v = applyEvent(emptyView(), ev("execution.cancelling"));
    expect(v.status).toBe("cancelling");
    v = applyEvent(v, ev("node.cancelled", "a"));
    v = applyEvent(v, ev("execution.cancelled"));
    expect(v.status).toBe("cancelled");
    expect(v.nodes.a.state).toBe("cancelled");
  });
});

describe("fromDetail", () => {
  const detail: ExecutionDetail = {
    execution: { id: "x", status: "failed" } as ExecutionDetail["execution"],
    nodes: [
      { node_id: "a", node_type: "log", state: "succeeded", attempt: 1 },
      { node_id: "b", node_type: "http_request", state: "failed", attempt: 3, worker_id: "w2" },
    ],
    attempts: [
      { attempt: 1, node_id: "b", status: "failed", delivery_count: 1, created_at: "" },
      { attempt: 2, node_id: "b", status: "failed", delivery_count: 1, created_at: "" },
      { attempt: 3, node_id: "b", status: "failed", delivery_count: 1, created_at: "" },
    ],
    children: [],
  };
  it("seeds node state and retries from REST data", () => {
    const v = fromDetail(detail);
    expect(v.status).toBe("failed");
    expect(v.nodes.b).toMatchObject({ state: "failed", retries: 2, worker: "w2" });
  });
  it("lets the REST status win over replayed events", () => {
    const v = fromDetail(detail, [ev("execution.started")]);
    expect(v.status).toBe("failed");
    expect(v.lastEventId).toBeGreaterThan(0);
  });
});

describe("timeline and formatting", () => {
  it("builds offsets and readable labels", () => {
    const rows = buildTimeline([
      { id: 1, execution_id: "x", type: "execution.started", created_at: "2024-01-01T00:00:00.000Z" },
      { id: 2, execution_id: "x", type: "node.retrying", node_id: "a", created_at: "2024-01-01T00:00:01.500Z", data: { delay_ms: 2000, error: { code: "http_error", message: "boom" } } },
    ]);
    expect(rows[0].offsetMs).toBe(0);
    expect(rows[1].offsetMs).toBe(1500);
    expect(rows[1].label).toBe("a retrying in 2.00s: boom");
    expect(rows[1].tone).toBe("warn");
  });
  it("formats durations", () => {
    expect(formatDuration(null)).toBe("–");
    expect(formatDuration(0.2)).toBe("<1ms");
    expect(formatDuration(340)).toBe("340ms");
    expect(formatDuration(2500)).toBe("2.50s");
    expect(formatDuration(75_000)).toBe("1m 15s");
  });
  it("computes durations against now for open intervals", () => {
    expect(durationMs("2024-01-01T00:00:00Z", "2024-01-01T00:00:02Z")).toBe(2000);
    expect(durationMs("2024-01-01T00:00:00Z", undefined, Date.parse("2024-01-01T00:00:05Z"))).toBe(5000);
    expect(durationMs(undefined, undefined)).toBeNull();
  });
});
