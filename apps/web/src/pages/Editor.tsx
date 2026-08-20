import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { WebhookInfo } from "@protocol";
import { api, ApiError } from "../lib/api";
import { atLeast, useAuth, useWorkspaceId } from "../state/auth";
import { useEditor } from "../state/editor";
import { Canvas } from "../components/Canvas";
import { Palette } from "../components/Palette";
import { ConfigPanel } from "../components/ConfigPanel";
import { ErrorBanner, Modal, StatusBadge, errMsg, shortId } from "../components/ui";
import { useExecution } from "../lib/useExecution";
import { formatDuration, durationMs } from "../lib/execution";

export function Editor() {
  const { id = "" } = useParams();
  const ws = useWorkspaceId();
  const { workspace } = useAuth();
  const ed = useEditor();
  const canEdit = atLeast(workspace?.role, "member");
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [runOpen, setRunOpen] = useState(false);
  const [runId, setRunId] = useState<string | null>(null);
  const [hooks, setHooks] = useState<WebhookInfo[]>([]);

  useEffect(() => {
    let alive = true;
    ed.reset();
    setRunId(null);
    api
      .workflow(ws, id)
      .then((w) => alive && ed.load(w))
      .catch((e) => alive && setLoadErr(e instanceof ApiError && e.status === 404 ? "Workflow not found" : errMsg(e)));
    return () => {
      alive = false;
      ed.reset();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ws, id]);

  useEffect(() => {
    if (!ed.publishedVersion) return setHooks([]);
    api
      .webhooks(ws, id)
      .then((r) => setHooks(r.webhooks))
      .catch(() => setHooks([]));
  }, [ws, id, ed.publishedVersion]);

  const live = useExecution(ws, runId);
  const selected = ed.nodes.find((n) => n.id === ed.selected)?.data.node ?? null;

  const save = useCallback(async (): Promise<boolean> => {
    setBusy("save");
    setMsg(null);
    try {
      const w = await api.updateWorkflow(ws, id, { name: ed.name, graph: ed.graph(), revision: ed.revision });
      ed.saved(w);
      setMsg({ tone: "ok", text: "Saved" });
      return true;
    } catch (e) {
      setMsg({ tone: "error", text: e instanceof ApiError && e.status === 409 ? "This workflow changed elsewhere. Reload to get the latest revision." : errMsg(e) });
      return false;
    } finally {
      setBusy(null);
    }
  }, [ws, id, ed]);

  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        if (canEdit) void save();
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [save, canEdit]);

  const validate = async () => {
    setBusy("validate");
    try {
      const r = await api.validate(ws, id, ed.graph());
      ed.setIssues(r.issues);
      setMsg(r.valid ? { tone: "ok", text: r.issues.length ? "Valid, with warnings" : "Valid" } : { tone: "error", text: "Validation found errors" });
    } catch (e) {
      setMsg({ tone: "error", text: errMsg(e) });
    } finally {
      setBusy(null);
    }
  };

  const publish = async () => {
    if (ed.dirty && !(await save())) return;
    setBusy("publish");
    try {
      const v = await api.publish(ws, id, "");
      ed.setIssues([]);
      const w = await api.workflow(ws, id);
      ed.saved(w);
      setMsg({ tone: "ok", text: `Published v${v.version}` });
    } catch (e) {
      if (e instanceof ApiError && Array.isArray((e.details as { issues?: unknown })?.issues)) ed.setIssues((e.details as { issues: never[] }).issues);
      setMsg({ tone: "error", text: errMsg(e) });
    } finally {
      setBusy(null);
    }
  };

  const run = async (trigger: unknown) => {
    setRunOpen(false);
    if (ed.dirty && !(await save())) return;
    setBusy("run");
    try {
      const r = await api.run(ws, id, trigger);
      setRunId(r.execution.id);
      setMsg({ tone: "ok", text: `Started ${shortId(r.execution.id)}` });
    } catch (e) {
      setMsg({ tone: "error", text: errMsg(e) });
    } finally {
      setBusy(null);
    }
  };

  if (loadErr) return <div className="page"><ErrorBanner error={loadErr} /><Link to="/workflows">Back to workflows</Link></div>;
  if (!ed.workflowId) return <div className="page muted">Loading…</div>;

  const graphIssues = ed.issues;
  const errorIssues = graphIssues.filter((i) => i.severity === "error");
  return (
    <div className="editor">
      <header className="toolbar">
        <Link to="/workflows" className="back" aria-label="Back to workflows">
          ←
        </Link>
        <input className="title-input" aria-label="Workflow name" value={ed.name} disabled={!canEdit} onChange={(e) => ed.setName(e.target.value)} />
        <StatusBadge status={ed.publishedVersion ? "published" : "draft"} />
        {ed.publishedVersion && <span className="muted">v{ed.publishedVersion}</span>}
        {ed.dirty && <span className="dirty">unsaved</span>}
        <div className="spacer" />
        {msg && (
          <span className={`toast ${msg.tone}`} role="status">
            {msg.text}
          </span>
        )}
        <Link to={`/workflows/${id}/versions`} className="btn ghost">
          Versions
        </Link>
        <button onClick={validate} disabled={busy !== null}>
          Validate
        </button>
        {canEdit && (
          <>
            <button onClick={() => void save()} disabled={busy !== null || !ed.dirty}>
              Save
            </button>
            <button onClick={publish} disabled={busy !== null}>
              Publish
            </button>
            <button className="primary" onClick={() => setRunOpen(true)} disabled={busy !== null || !ed.publishedVersion} title={ed.publishedVersion ? "" : "Publish first"}>
              Run
            </button>
          </>
        )}
      </header>
      <div className="editor-body">
        {canEdit && <Palette onAdd={(t) => ed.addNode(t, { x: 80 + ed.nodes.length * 30, y: 80 + ed.nodes.length * 30 })} />}
        <div className="canvas-col">
          <Canvas
            nodes={ed.nodes}
            edges={ed.edges}
            view={runId ? live.view : null}
            readOnly={!canEdit}
            onNodesChange={ed.onNodesChange}
            onEdgesChange={ed.onEdgesChange}
            onConnect={ed.connect}
            onDropNode={ed.addNode}
            onSelect={ed.select}
          />
          {runId && (
            <div className="run-strip" role="status">
              <strong>Run {shortId(runId)}</strong>
              <StatusBadge status={live.view.status} />
              <span className="muted">
                {formatDuration(durationMs(live.detail?.execution.started_at ?? live.detail?.execution.created_at, live.detail?.execution.finished_at))}
              </span>
              <span className="muted">stream: {live.stream}</span>
              <Link to={`/executions/${runId}`}>Open debugger</Link>
              <button className="ghost" onClick={() => setRunId(null)}>
                Dismiss
              </button>
            </div>
          )}
          {graphIssues.length > 0 && (
            <div className="issues" role="list" aria-label="Validation issues">
              <h4>
                {errorIssues.length} error{errorIssues.length === 1 ? "" : "s"}, {graphIssues.length - errorIssues.length} warning(s)
              </h4>
              {graphIssues.map((i, k) => (
                <button key={k} role="listitem" className={`issue ${i.severity}`} onClick={() => i.node_id && ed.select(i.node_id)}>
                  <span className="issue-sev">{i.severity}</span>
                  {i.node_id && <code>{i.node_id}</code>} {i.message}
                </button>
              ))}
            </div>
          )}
          {hooks.length > 0 && (
            <div className="hooks">
              <h4>Webhooks</h4>
              {hooks.map((h) => (
                <div key={h.id}>
                  <code>POST {location.origin}/hooks/{h.id}</code> <span className="muted">{h.node_id}{h.signed ? " · signed" : ""}</span>
                </div>
              ))}
            </div>
          )}
        </div>
        {selected && (
          <ConfigPanel
            node={selected}
            readOnly={!canEdit}
            onConfig={(k, v) => ed.patchConfig(selected.id, k, v)}
            onPatch={(p) => ed.patchNode(selected.id, p)}
            onDelete={() => ed.deleteNode(selected.id)}
          />
        )}
      </div>
      {runOpen && <RunDialog sample={sampleFor(ed)} onClose={() => setRunOpen(false)} onRun={run} />}
    </div>
  );
}

function sampleFor(ed: ReturnType<typeof useEditor.getState>): string {
  const t = ed.nodes.find((n) => n.data.node.type === "manual_trigger");
  const s = t?.data.node.config?.sample;
  return s === undefined ? "{}" : JSON.stringify(s, null, 2);
}

function RunDialog({ sample, onClose, onRun }: { sample: string; onClose: () => void; onRun: (t: unknown) => void }) {
  const [text, setText] = useState(sample);
  const [err, setErr] = useState<string | null>(null);
  return (
    <Modal title="Run workflow" onClose={onClose}>
      <p className="muted">Trigger payload (JSON) is available to expressions as <code>trigger</code>.</p>
      <textarea className="mono" rows={10} value={text} onChange={(e) => setText(e.target.value)} aria-label="Trigger payload" spellCheck={false} />
      <ErrorBanner error={err} />
      <footer className="modal-actions">
        <button onClick={onClose}>Cancel</button>
        <button
          className="primary"
          onClick={() => {
            try {
              onRun(text.trim() ? JSON.parse(text) : {});
            } catch {
              setErr("Payload is not valid JSON");
            }
          }}
        >
          Run
        </button>
      </footer>
    </Modal>
  );
}

