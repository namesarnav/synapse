import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import type { Graph, NodeExec } from "@protocol";
import { isTerminalExec } from "@protocol";
import { api } from "../lib/api";
import { atLeast, useAuth, useWorkspaceId } from "../state/auth";
import { useExecution } from "../lib/useExecution";
import { Canvas } from "../components/Canvas";
import { Empty, ErrorBanner, Json, StatusBadge, ago, errMsg, shortId } from "../components/ui";
import { buildTimeline, durationMs, formatDuration } from "../lib/execution";
import { toFlow } from "../lib/graph";

export function ExecutionDetail() {
  const { id = "" } = useParams();
  const ws = useWorkspaceId();
  const { workspace } = useAuth();
  const nav = useNavigate();
  const canAct = atLeast(workspace?.role, "member");
  const live = useExecution(ws, id);
  const { detail, view } = live;
  const [graph, setGraph] = useState<Graph | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [now, setNow] = useState(Date.now());

  const wfId = detail?.execution.workflow_id;
  const version = detail?.execution.version;
  useEffect(() => {
    if (!wfId || !version) return;
    let alive = true;
    api
      .version(ws, wfId, version)
      .then((v) => alive && setGraph(v.graph ?? null))
      .catch((e) => alive && setActionErr(errMsg(e)));
    return () => {
      alive = false;
    };
  }, [ws, wfId, version]);

  const running = !isTerminalExec(view.status);
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [running]);

  const flow = useMemo(() => (graph ? toFlow(graph) : { nodes: [], edges: [] }), [graph]);
  const timeline = useMemo(() => buildTimeline(view.events), [view.events]);
  const nodeDetail: NodeExec | undefined = detail?.nodes.find((n) => n.node_id === selected);
  const attempts = detail?.attempts.filter((a) => a.node_id === selected) ?? [];

  const act = async (fn: () => Promise<{ execution: { id: string } } | void>, go = false) => {
    setActionErr(null);
    try {
      const r = await fn();
      if (go && r) nav(`/executions/${r.execution.id}`);
      else await live.reload();
    } catch (e) {
      setActionErr(errMsg(e));
    }
  };

  if (live.error && !detail) return <div className="page"><ErrorBanner error={live.error} /><Link to="/executions">Back</Link></div>;
  if (!detail) return <div className="page muted">Loading…</div>;
  const ex = detail.execution;
  const finished = isTerminalExec(view.status);

  return (
    <div className="page debugger">
      <header className="page-head">
        <div>
          <h2>
            Execution <code>{shortId(ex.id)}</code>
          </h2>
          <div className="row meta">
            <StatusBadge status={view.status} />
            <Link to={`/workflows/${ex.workflow_id}`}>workflow</Link>
            <span>v{ex.version}</span>
            <span>{ex.trigger_type}</span>
            <span>{formatDuration(durationMs(ex.started_at ?? ex.created_at, ex.finished_at, now))}</span>
            <span className="muted">created {ago(ex.created_at, now)}</span>
            <span className="muted">stream: {live.stream}</span>
          </div>
          {ex.replay_of && (
            <div className="muted">
              Replay of <Link to={`/executions/${ex.replay_of}`}>{shortId(ex.replay_of)}</Link>
              {ex.replay_source_node ? ` from node ${ex.replay_source_node}` : " (full)"}
            </div>
          )}
        </div>
        {canAct && (
          <div className="row">
            {!finished && <button onClick={() => act(() => api.cancel(ws, id))}>Cancel</button>}
            {finished && (
              <button className="primary" onClick={() => act(() => api.replay(ws, id), true)}>
                Replay execution
              </button>
            )}
          </div>
        )}
      </header>
      <ErrorBanner error={actionErr} />
      {ex.error && <ErrorBanner error={ex.error} />}

      <div className="debug-grid">
        <section className="debug-canvas">
          {graph ? <Canvas nodes={flow.nodes} edges={flow.edges} view={view} readOnly onSelect={setSelected} /> : <div className="muted">Loading graph…</div>}
        </section>

        <section className="debug-side">
          <h3>Nodes</h3>
          <ul className="node-list">
            {detail.nodes.map((n) => {
              const nv = view.nodes[n.node_id];
              return (
                <li key={n.node_id}>
                  <button className={selected === n.node_id ? "active" : ""} onClick={() => setSelected(n.node_id)}>
                    <StatusBadge status={nv?.state ?? n.state} />
                    <span className="nl-name">{n.node_id}</span>
                    <span className="muted">{formatDuration(durationMs(nv?.startedAt ?? n.started_at, nv?.finishedAt ?? n.finished_at, now))}</span>
                    {nv && nv.retries > 0 && <span className="tag">{nv.retries} retr{nv.retries === 1 ? "y" : "ies"}</span>}
                  </button>
                </li>
              );
            })}
          </ul>

          {selected && nodeDetail ? (
            <div className="node-detail">
              <h3>
                {selected} <span className="muted">{nodeDetail.node_type}</span>
              </h3>
              <dl>
                <dt>State</dt>
                <dd>
                  <StatusBadge status={view.nodes[selected]?.state ?? nodeDetail.state} />
                </dd>
                <dt>Worker</dt>
                <dd>{view.nodes[selected]?.worker ?? nodeDetail.worker_id ?? "–"}</dd>
                <dt>Latency</dt>
                <dd>{formatDuration(durationMs(nodeDetail.started_at, nodeDetail.finished_at, now))}</dd>
                {nodeDetail.branch && (
                  <>
                    <dt>Branch</dt>
                    <dd>{nodeDetail.branch}</dd>
                  </>
                )}
                {view.nodes[selected]?.wakeAt && (
                  <>
                    <dt>Wakes at</dt>
                    <dd>{view.nodes[selected]?.wakeAt}</dd>
                  </>
                )}
              </dl>
              {nodeDetail.error && (
                <div className="banner error">
                  <strong>{nodeDetail.error.code}</strong>: {nodeDetail.error.message}
                </div>
              )}
              <h4>Input</h4>
              <Json value={nodeDetail.input} />
              <h4>Output</h4>
              <Json value={nodeDetail.output} />
              <h4>Attempts</h4>
              {attempts.length === 0 ? (
                <span className="muted">none</span>
              ) : (
                <table className="table compact">
                  <thead>
                    <tr>
                      <th>#</th>
                      <th>Status</th>
                      <th>Deliveries</th>
                      <th>Worker</th>
                      <th>Time</th>
                      <th>Error</th>
                    </tr>
                  </thead>
                  <tbody>
                    {attempts.map((a) => (
                      <tr key={a.attempt}>
                        <td>{a.attempt}</td>
                        <td>{a.status}</td>
                        <td>{a.delivery_count}</td>
                        <td>{a.worker_id ?? "–"}</td>
                        <td>{formatDuration(durationMs(a.started_at, a.finished_at, now))}</td>
                        <td>{a.error ? `${a.error.code}: ${a.error.message}` : ""}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {canAct && finished && (
                <button onClick={() => act(() => api.replay(ws, id, selected), true)} title="Reuse earlier results and rerun from this node">
                  Replay from this node
                </button>
              )}
            </div>
          ) : (
            <p className="muted">Select a node to see its input, output and attempts.</p>
          )}
        </section>
      </div>

      <section>
        <h3>Event timeline</h3>
        {timeline.length === 0 ? (
          <Empty title="No events yet" />
        ) : (
          <ol className="timeline">
            {timeline.map((r) => (
              <li key={r.id} className={`tl-${r.tone}`}>
                <span className="tl-time">+{formatDuration(r.offsetMs)}</span>
                <span className="tl-label">{r.label}</span>
              </li>
            ))}
          </ol>
        )}
      </section>

      {detail.children.length > 0 && (
        <section>
          <h3>Child executions</h3>
          <ul>
            {detail.children.map((c) => (
              <li key={c.id}>
                <Link to={`/executions/${c.id}`}>{shortId(c.id)}</Link> <StatusBadge status={c.status} />
              </li>
            ))}
          </ul>
        </section>
      )}

      <section className="io">
        <div>
          <h3>Trigger payload</h3>
          <Json value={ex.trigger_payload} />
        </div>
        <div>
          <h3>Output</h3>
          <Json value={ex.output} />
        </div>
      </section>
    </div>
  );
}
