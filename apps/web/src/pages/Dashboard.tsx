import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import type { ExecutionSummary, WorkflowSummary } from "@protocol";
import { api } from "../lib/api";
import { streamWorkspace } from "../lib/stream";
import { useWorkspaceId } from "../state/auth";
import { Empty, ErrorBanner, StatusBadge, ago, errMsg, shortId } from "../components/ui";
import { durationMs, formatDuration } from "../lib/execution";

export function Dashboard() {
  const ws = useWorkspaceId();
  const [execs, setExecs] = useState<ExecutionSummary[] | null>(null);
  const [wfs, setWfs] = useState<WorkflowSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    const load = () =>
      Promise.all([api.executions(ws, { limit: 100 }), api.workflows(ws, { limit: 100 })])
        .then(([e, w]) => {
          if (!alive) return;
          setExecs(e.items);
          setWfs(w.items);
        })
        .catch((e) => alive && setError(errMsg(e)));
    void load();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const stop = streamWorkspace(ws, () => {
      clearTimeout(timer);
      timer = setTimeout(load, 300);
    });
    return () => {
      alive = false;
      clearTimeout(timer);
      stop();
    };
  }, [ws]);

  const count = (s: string) => execs?.filter((e) => e.status === s).length ?? 0;
  const names = new Map(wfs?.map((w) => [w.id, w.name]));

  return (
    <div className="page">
      <header className="page-head">
        <h2>Dashboard</h2>
      </header>
      <ErrorBanner error={error} />
      <section className="stats" aria-label="Execution summary">
        <Stat label="Workflows" value={wfs?.length} />
        <Stat label="Running" value={execs ? count("running") + count("waiting") + count("created") : undefined} tone="info" />
        <Stat label="Succeeded" value={execs ? count("succeeded") : undefined} tone="ok" />
        <Stat label="Failed" value={execs ? count("failed") : undefined} tone="bad" />
      </section>
      <h3>Recent executions</h3>
      {execs && execs.length === 0 ? (
        <Empty title="No executions yet">
          <p>
            <Link to="/workflows">Open a workflow</Link>, publish it and run it to see activity here.
          </p>
        </Empty>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Execution</th>
              <th>Workflow</th>
              <th>Status</th>
              <th>Trigger</th>
              <th>Duration</th>
              <th>Started</th>
            </tr>
          </thead>
          <tbody>
            {execs?.slice(0, 10).map((e) => (
              <tr key={e.id}>
                <td>
                  <Link to={`/executions/${e.id}`}>{shortId(e.id)}</Link>
                </td>
                <td>{names.get(e.workflow_id) ?? shortId(e.workflow_id)}</td>
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
    </div>
  );
}

function Stat({ label, value, tone }: { label: string; value?: number; tone?: string }) {
  return (
    <div className={`stat ${tone ?? ""}`}>
      <div className="stat-value">{value ?? "–"}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}
