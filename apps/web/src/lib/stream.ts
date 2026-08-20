import type { ExecEvent, WsMessage } from "@protocol";
import { tokenStore } from "./api";

export type StreamStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface StreamOptions {
  after?: number;
  onEvent: (e: ExecEvent) => void;
  onEnd?: (status: string) => void;
  onStatus?: (s: StreamStatus) => void;
  /** Injectable for tests. */
  WebSocketImpl?: typeof WebSocket;
  baseUrl?: string;
  maxBackoffMs?: number;
}

const wsBase = () => `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}`;

/**
 * Streams one execution's events. It reconnects with exponential backoff and
 * resumes from the last event id it saw, so a dropped socket or a slow-client
 * eviction loses nothing. Returns a function that stops the stream.
 */
export function streamExecution(id: string, opts: StreamOptions): () => void {
  const WS = opts.WebSocketImpl ?? WebSocket;
  let last = opts.after ?? 0;
  let stopped = false;
  let finished = false;
  let attempt = 0;
  let sock: WebSocket | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const connect = () => {
    if (stopped) return;
    opts.onStatus?.(attempt === 0 ? "connecting" : "reconnecting");
    const token = tokenStore.get();
    const url = `${opts.baseUrl ?? wsBase()}/ws/executions/${id}?after=${last}${token ? `&token=${encodeURIComponent(token)}` : ""}`;
    const s = new WS(url);
    sock = s;
    s.onmessage = (ev: MessageEvent) => {
      let m: WsMessage;
      try {
        m = JSON.parse(String(ev.data)) as WsMessage;
      } catch {
        return;
      }
      switch (m.type) {
        case "hello":
          attempt = 0;
          opts.onStatus?.("live");
          break;
        case "event":
          if (m.event && m.event.id > last) {
            last = m.event.id;
            opts.onEvent(m.event);
          }
          break;
        case "resync":
          if (m.last_event_id && m.last_event_id > last) last = m.last_event_id;
          break;
        case "end":
          finished = true;
          opts.onEnd?.(m.status ?? "");
          break;
      }
    };
    s.onclose = () => {
      sock = null;
      if (stopped || finished) {
        opts.onStatus?.("closed");
        return;
      }
      const max = opts.maxBackoffMs ?? 10_000;
      const delay = Math.min(max, 250 * 2 ** attempt) * (0.75 + Math.random() * 0.5);
      attempt++;
      opts.onStatus?.("reconnecting");
      timer = setTimeout(connect, delay);
    };
    s.onerror = () => s.close();
  };
  connect();
  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
    sock?.close();
  };
}

/** Streams execution-level events for a workspace; callers refetch on `onEvent`. */
export function streamWorkspace(ws: string, onEvent: (e: ExecEvent) => void, WebSocketImpl: typeof WebSocket = WebSocket): () => void {
  let stopped = false;
  let attempt = 0;
  let sock: WebSocket | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const connect = () => {
    if (stopped) return;
    const token = tokenStore.get();
    const s = new WebSocketImpl(`${wsBase()}/ws/workspaces/${ws}${token ? `?token=${encodeURIComponent(token)}` : ""}`);
    sock = s;
    s.onmessage = (ev: MessageEvent) => {
      try {
        const m = JSON.parse(String(ev.data)) as WsMessage;
        if (m.type === "hello") attempt = 0;
        if (m.type === "event" && m.event) onEvent(m.event);
        if (m.type === "resync") onEvent({ id: 0, execution_id: "", type: "execution.created", created_at: "" });
      } catch {
        /* ignore malformed frame */
      }
    };
    s.onclose = () => {
      if (stopped) return;
      timer = setTimeout(connect, Math.min(10_000, 500 * 2 ** attempt++));
    };
    s.onerror = () => s.close();
  };
  connect();
  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
    sock?.close();
  };
}
