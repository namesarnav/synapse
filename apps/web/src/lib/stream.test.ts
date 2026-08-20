import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExecEvent, WsMessage } from "@protocol";
import { streamExecution } from "./stream";
import { tokenStore } from "./api";

class FakeWS {
  static all: FakeWS[] = [];
  onmessage: ((e: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(public url: string) {
    FakeWS.all.push(this);
  }
  send(m: WsMessage) {
    this.onmessage?.({ data: JSON.stringify(m) } as MessageEvent);
  }
  close() {
    this.closed = true;
    this.onclose?.();
  }
}
const impl = FakeWS as unknown as typeof WebSocket;
const event = (id: number): ExecEvent => ({ id, execution_id: "x", type: "node.started", node_id: "a", created_at: "" });

describe("streamExecution", () => {
  beforeEach(() => {
    FakeWS.all = [];
    localStorage.clear();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("connects with the resume cursor and token", () => {
    tokenStore.set("tok");
    streamExecution("x", { after: 7, onEvent: () => undefined, WebSocketImpl: impl, baseUrl: "ws://h" });
    expect(FakeWS.all[0].url).toBe("ws://h/ws/executions/x?after=7&token=tok");
  });

  it("delivers events once and drops replays", () => {
    const got: number[] = [];
    streamExecution("x", { onEvent: (e) => got.push(e.id), WebSocketImpl: impl, baseUrl: "ws://h" });
    const s = FakeWS.all[0];
    s.send({ type: "hello" });
    s.send({ type: "event", event: event(1) });
    s.send({ type: "event", event: event(2) });
    s.send({ type: "event", event: event(2) });
    s.send({ type: "event", event: event(1) });
    expect(got).toEqual([1, 2]);
  });

  it("reconnects after a drop and resumes from the last event id", () => {
    const statuses: string[] = [];
    streamExecution("x", { onEvent: () => undefined, onStatus: (s) => statuses.push(s), WebSocketImpl: impl, baseUrl: "ws://h" });
    FakeWS.all[0].send({ type: "event", event: event(5) });
    FakeWS.all[0].onclose?.();
    expect(statuses.at(-1)).toBe("reconnecting");
    vi.advanceTimersByTime(1000);
    expect(FakeWS.all).toHaveLength(2);
    expect(FakeWS.all[1].url).toContain("after=5");
  });

  it("does not reconnect after the end message", () => {
    const end = vi.fn();
    streamExecution("x", { onEvent: () => undefined, onEnd: end, WebSocketImpl: impl, baseUrl: "ws://h" });
    FakeWS.all[0].send({ type: "end", status: "succeeded" });
    FakeWS.all[0].onclose?.();
    vi.advanceTimersByTime(60_000);
    expect(end).toHaveBeenCalledWith("succeeded");
    expect(FakeWS.all).toHaveLength(1);
  });

  it("stops cleanly and cancels a pending reconnect", () => {
    const stop = streamExecution("x", { onEvent: () => undefined, WebSocketImpl: impl, baseUrl: "ws://h" });
    FakeWS.all[0].onclose?.();
    stop();
    vi.advanceTimersByTime(60_000);
    expect(FakeWS.all).toHaveLength(1);
  });

  it("backs off further on repeated failures", () => {
    streamExecution("x", { onEvent: () => undefined, WebSocketImpl: impl, baseUrl: "ws://h", maxBackoffMs: 8000 });
    for (let i = 0; i < 4; i++) {
      FakeWS.all.at(-1)!.onclose?.();
      vi.advanceTimersByTime(20_000);
    }
    expect(FakeWS.all).toHaveLength(5);
  });
});
