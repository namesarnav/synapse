import { useCallback, useEffect, useRef, useState } from "react";
import type { ExecutionDetail } from "@protocol";
import { isTerminalExec } from "@protocol";
import { api } from "./api";
import { applyEvent, emptyView, fromDetail, type ExecView } from "./execution";
import { streamExecution, type StreamStatus } from "./stream";

export interface LiveExecution {
  detail: ExecutionDetail | null;
  view: ExecView;
  stream: StreamStatus;
  error: string | null;
  reload: () => Promise<number>;
}

/** Loads an execution, then keeps it current from the WebSocket stream. */
export function useExecution(ws: string, id: string | null): LiveExecution {
  const [detail, setDetail] = useState<ExecutionDetail | null>(null);
  const [view, setView] = useState<ExecView>(emptyView);
  const [stream, setStream] = useState<StreamStatus>("closed");
  const [error, setError] = useState<string | null>(null);
  const alive = useRef(true);

  const load = useCallback(async (): Promise<number> => {
    if (!id) return 0;
    try {
      const [d, ev] = await Promise.all([api.execution(ws, id), api.events(ws, id, 0)]);
      if (!alive.current) return 0;
      setDetail(d);
      const v = fromDetail(d, ev.events);
      setView(v);
      setError(null);
      return v.lastEventId;
    } catch (e) {
      if (alive.current) setError(e instanceof Error ? e.message : String(e));
    }
    return 0;
  }, [ws, id]);

  useEffect(() => {
    alive.current = true;
    setDetail(null);
    setView(emptyView());
    if (!id) return;
    let stop: (() => void) | undefined;
    let cancelled = false;
    (async () => {
      const after = await load();
      if (cancelled) return;
      stop = streamExecution(id, {
        after,
        onEvent: (e) => setView((v) => applyEvent(v, e)),
        onStatus: setStream,
        onEnd: () => void load(),
      });
    })();
    return () => {
      alive.current = false;
      cancelled = true;
      stop?.();
    };
  }, [id, load]);

  // A terminal status seen on the stream means the REST detail (output, attempts) is stale.
  const status = view.status;
  const seen = useRef<string>("");
  useEffect(() => {
    if (isTerminalExec(status) && seen.current !== `${id}:${status}`) {
      seen.current = `${id}:${status}`;
      void load();
    }
  }, [status, id, load]);

  return { detail, view, stream, error, reload: load };
}
