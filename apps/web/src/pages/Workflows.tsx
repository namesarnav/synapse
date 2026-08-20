import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import type { WorkflowSummary } from "@protocol";
import { api } from "../lib/api";
import { atLeast, useAuth, useWorkspaceId } from "../state/auth";
import { Empty, ErrorBanner, StatusBadge, ago, errMsg } from "../components/ui";

export function Workflows() {
  const ws = useWorkspaceId();
  const { workspace } = useAuth();
  const nav = useNavigate();
  const [items, setItems] = useState<WorkflowSummary[] | null>(null);
  const [query, setQuery] = useState("");
  const [error, setError] = useState<string | null>(null);
  const canEdit = atLeast(workspace?.role, "member");

  const load = () =>
    api
      .workflows(ws, { limit: 100 })
      .then((r) => setItems(r.items))
      .catch((e) => setError(errMsg(e)));
  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ws]);

  const create = async () => {
    try {
      const w = await api.createWorkflow(ws, "Untitled workflow", { nodes: [], edges: [] });
      nav(`/workflows/${w.id}`);
    } catch (e) {
      setError(errMsg(e));
    }
  };
  const remove = async (id: string, name: string) => {
    if (!window.confirm(`Delete "${name}"? Its execution history is kept only until the workflow is removed.`)) return;
    try {
      await api.deleteWorkflow(ws, id);
      await load();
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const shown = items?.filter((w) => w.name.toLowerCase().includes(query.toLowerCase()));
  return (
    <div className="page">
      <header className="page-head">
        <h2>Workflows</h2>
        <div className="row">
          <input type="search" placeholder="Filter…" aria-label="Filter workflows" value={query} onChange={(e) => setQuery(e.target.value)} />
          {canEdit && (
            <button className="primary" onClick={create}>
              New workflow
            </button>
          )}
        </div>
      </header>
      <ErrorBanner error={error} />
      {shown && shown.length === 0 ? (
        <Empty title={items?.length ? "No matches" : "No workflows yet"}>{canEdit && !items?.length && <p>Create one to start building.</p>}</Empty>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Status</th>
              <th>Published</th>
              <th>Nodes</th>
              <th>Updated</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {shown?.map((w) => (
              <tr key={w.id}>
                <td>
                  <Link to={`/workflows/${w.id}`}>{w.name}</Link>
                </td>
                <td>
                  <StatusBadge status={w.published_version ? "published" : "draft"} />
                </td>
                <td>{w.published_version ? `v${w.published_version}` : "–"}</td>
                <td>{w.node_count}</td>
                <td>{ago(w.updated_at)}</td>
                <td className="actions">
                  <Link to={`/workflows/${w.id}/versions`}>Versions</Link>
                  <Link to={`/executions?workflow_id=${w.id}`}>Runs</Link>
                  {canEdit && (
                    <button className="ghost danger" onClick={() => remove(w.id, w.name)}>
                      Delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
