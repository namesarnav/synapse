import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import type { ExecutionSummary, WorkflowSummary } from "@protocol";
import { EXEC_STATES } from "@protocol";
import { api } from "../lib/api";
import { streamWorkspace } from "../lib/stream";
import { useWorkspaceId } from "../state/auth";
import { Empty, ErrorBanner, StatusBadge, ago, errMsg, shortId } from "../components/ui";
import { durationMs, formatDuration } from "../lib/execution";

export function Executions() {
  const ws = useWorkspaceId();
  const [params, setParams] = useSearchParams();
  const status = params.get("status") ?? "";
  const workflowId = params.get("workflow_id") ?? "";
  const [items, setItems] = useState<ExecutionSummary[] | null>(null);
  const [cursor, setCursor] = useState("");
  const [wfs, setWfs] = useState<WorkflowSummary[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.workflows(ws, { limit: 100 }).then((r) => setWfs(r.items)).catch(() => undefined);
  }, [ws]);

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .executions(ws, { status, workflow_id: workflowId, limit: 50 })
        .then((r) => {
          if (!alive) return;
          setItems(r.items);
          setCursor(r.next_cursor);
        })
        .catch((e) => alive && setError(errMsg(e)));
    void load();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const stop = streamWorkspace(ws, () => {
      clearTimeout(timer);
      timer = setTimeout(load, 400);
    });
    return () => {
      alive = false;
      clearTimeout(timer);
      stop();
    };
  }, [ws, status, workflowId]);

  const more = async () => {
    try {
      const r = await api.executions(ws, { status, workflow_id: workflowId, limit: 50, cursor });
      setItems((cur) => [...(cur ?? []), ...r.items]);
      setCursor(r.next_cursor);
    } catch (e) {
      setError(errMsg(e));
    }
  };
  const setParam = (k: string, v: string) => {
    const n = new URLSearchParams(params);
    if (v) n.set(k, v);
    else n.delete(k);
    setParams(n, { replace: true });
  };
  const names = new Map(wfs.map((w) => [w.id, w.name]));

  return (
    <div className="page">
      <header className="page-head">
        <h2>Executions</h2>
        <div className="row">
          <select aria-label="Workflow" value={workflowId} onChange={(e) => setParam("workflow_id", e.target.value)}>
            <option value="">All workflows</option>
            {wfs.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
          <select aria-label="Status" value={status} onChange={(e) => setParam("status", e.target.value)}>
            <option value="">Any status</option>
            {EXEC_STATES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
      </header>
      <ErrorBanner error={error} />
      {items && items.length === 0 ? (
        <Empty title="No executions match" />
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Execution</th>
              <th>Workflow</th>
              <th>Version</th>
              <th>Status</th>
              <th>Trigger</th>
              <th>Duration</th>
              <th>Created</th>
            </tr>
          </thead>
          <tbody>
            {items?.map((e) => (
              <tr key={e.id}>
                <td>
                  <Link to={`/executions/${e.id}`}>{shortId(e.id)}</Link>
                  {e.replay_of && <span className="tag" title={`replay of ${e.replay_of}`}>replay</span>}
                </td>
                <td>{names.get(e.workflow_id) ?? shortId(e.workflow_id)}</td>
                <td>v{e.version}</td>
                <td>
                  <StatusBadge status={e.status} />
                </td>
                <td>{e.trigger_type}</td>
                <td>{formatDuration(durationMs(e.started_at, e.finished_at))}</td>
                <td>{ago(e.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {cursor && (
        <button className="load-more" onClick={more}>
          Load more
        </button>
      )}
    </div>
  );
}
