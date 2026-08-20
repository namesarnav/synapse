import type { ExecEvent, ExecState, ExecutionDetail, NodeError, NodeState } from "@protocol";

export interface NodeView {
  id: string;
  state: NodeState;
  attempt: number;
  retries: number;
  startedAt?: string;
  finishedAt?: string;
  worker?: string;
  error?: NodeError;
  wakeAt?: string;
}

export interface ExecView {
  status: ExecState;
  nodes: Record<string, NodeView>;
  events: ExecEvent[];
  lastEventId: number;
}

export const emptyView = (): ExecView => ({ status: "created", nodes: {}, events: [], lastEventId: 0 });

const blank = (id: string): NodeView => ({ id, state: "pending", attempt: 0, retries: 0 });

/** Seeds a view from the REST detail; events applied afterwards refine it. */
export function fromDetail(d: ExecutionDetail, events: ExecEvent[] = []): ExecView {
  const nodes: Record<string, NodeView> = {};
  for (const n of d.nodes) {
    nodes[n.node_id] = {
      id: n.node_id,
      state: n.state,
      attempt: n.attempt,
      retries: Math.max(0, d.attempts.filter((a) => a.node_id === n.node_id).length - 1),
      startedAt: n.started_at,
      finishedAt: n.finished_at,
      worker: n.worker_id,
      error: n.error,
      wakeAt: n.wake_at,
    };
  }
  let v: ExecView = { status: d.execution.status, nodes, events: [], lastEventId: 0 };
  for (const e of events) v = applyEvent(v, e);
  v.status = d.execution.status;
  return v;
}

const execStatusFor: Partial<Record<ExecEvent["type"], ExecState>> = {
  "execution.created": "created",
  "execution.started": "running",
  "execution.succeeded": "succeeded",
  "execution.failed": "failed",
  "execution.cancelled": "cancelled",
  "execution.cancelling": "cancelling",
};

/** Applies one event. It is idempotent: events at or below lastEventId are ignored. */
export function applyEvent(v: ExecView, e: ExecEvent, keepEvent = true): ExecView {
  if (e.id <= v.lastEventId) return v;
  const next: ExecView = { status: v.status, nodes: v.nodes, events: keepEvent ? [...v.events, e] : v.events, lastEventId: e.id };
  const es = execStatusFor[e.type];
  if (es) next.status = es;
  if (!e.node_id) return next;
  const cur = v.nodes[e.node_id] ?? blank(e.node_id);
  const n: NodeView = { ...cur };
  if (e.attempt) n.attempt = e.attempt;
  switch (e.type) {
    case "node.queued":
      n.state = "queued";
      break;
    case "node.started":
      n.state = "running";
      n.startedAt = e.created_at;
      n.finishedAt = undefined;
      n.error = undefined;
      n.worker = typeof e.data?.worker === "string" ? e.data.worker : n.worker;
      break;
    case "node.succeeded":
      n.state = "succeeded";
      n.finishedAt = e.created_at;
      break;
    case "node.failed":
      n.state = "failed";
      n.finishedAt = e.created_at;
      n.error = e.data?.error as NodeError | undefined;
      break;
    case "node.retrying":
      n.state = "retrying";
      n.retries += 1;
      n.error = e.data?.error as NodeError | undefined;
      break;
    case "node.waiting":
      n.state = "waiting";
      n.wakeAt = typeof e.data?.wake_at === "string" ? e.data.wake_at : undefined;
      break;
    case "node.skipped":
      n.state = "skipped";
      n.finishedAt = e.created_at;
      break;
    case "node.cancelled":
      n.state = "cancelled";
      n.finishedAt = e.created_at;
      break;
    default:
      return next;
  }
  next.nodes = { ...v.nodes, [e.node_id]: n };
  return next;
}

/** Duration in ms between two timestamps, or null when either is missing. */
export function durationMs(start?: string, end?: string, now: number = Date.now()): number | null {
  if (!start) return null;
  const s = Date.parse(start);
  const e = end ? Date.parse(end) : now;
  return Number.isNaN(s) || Number.isNaN(e) ? null : Math.max(0, e - s);
}

export function formatDuration(ms: number | null): string {
  if (ms === null) return "–";
  if (ms < 1) return "<1ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)}s`;
  const m = Math.floor(ms / 60_000);
  return `${m}m ${Math.round((ms % 60_000) / 1000)}s`;
}

export interface TimelineRow {
  id: number;
  offsetMs: number;
  label: string;
  nodeId?: string;
  tone: "info" | "ok" | "bad" | "warn" | "muted";
}

const labels: Record<ExecEvent["type"], [string, TimelineRow["tone"]]> = {
  "execution.created": ["Execution created", "info"],
  "execution.started": ["Execution started", "info"],
  "execution.succeeded": ["Execution succeeded", "ok"],
  "execution.failed": ["Execution failed", "bad"],
  "execution.cancelled": ["Execution cancelled", "warn"],
  "execution.cancelling": ["Cancellation requested", "warn"],
  "node.queued": ["queued", "muted"],
  "node.started": ["started", "info"],
  "node.succeeded": ["succeeded", "ok"],
  "node.failed": ["failed", "bad"],
  "node.retrying": ["retrying", "warn"],
  "node.waiting": ["waiting", "info"],
  "node.skipped": ["skipped", "muted"],
  "node.cancelled": ["cancelled", "warn"],
  "node.log": ["log", "muted"],
};

/** Turns raw events into readable rows offset from the first event. */
export function buildTimeline(events: ExecEvent[]): TimelineRow[] {
  if (events.length === 0) return [];
  const t0 = Date.parse(events[0].created_at);
  return events.map((e) => {
    const [text, tone] = labels[e.type] ?? [e.type, "muted" as const];
    let label = e.node_id ? `${e.node_id} ${text}` : text;
    if (e.type === "node.started" && (e.attempt ?? 0) > 1) label += ` (attempt ${e.attempt})`;
    if (e.type === "node.retrying") {
      const d = e.data?.delay_ms;
      label += ` in ${formatDuration(typeof d === "number" ? d : null)}`;
    }
    const err = e.data?.error as NodeError | undefined;
    if (err?.message) label += `: ${err.message}`;
    return { id: e.id, offsetMs: Math.max(0, Date.parse(e.created_at) - t0), label, nodeId: e.node_id, tone };
  });
}

export const STATE_GLYPH: Record<NodeState, string> = {
  pending: "○",
  ready: "◔",
  queued: "◔",
  running: "◐",
  waiting: "⏸",
  succeeded: "✓",
  failed: "✕",
  retrying: "↻",
  skipped: "⤼",
  cancelled: "⊘",
};
