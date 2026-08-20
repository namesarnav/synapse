import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { WorkflowVersion } from "@protocol";
import { api } from "../lib/api";
import { useWorkspaceId } from "../state/auth";
import { Canvas } from "../components/Canvas";
import { Empty, ErrorBanner, ago, errMsg } from "../components/ui";
import { toFlow } from "../lib/graph";

export function Versions() {
  const { id = "" } = useParams();
  const ws = useWorkspaceId();
  const [items, setItems] = useState<WorkflowVersion[] | null>(null);
  const [open, setOpen] = useState<WorkflowVersion | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .versions(ws, id)
      .then((r) => setItems(r.items))
      .catch((e) => setError(errMsg(e)));
  }, [ws, id]);

  const view = async (v: number) => {
    try {
      setOpen(await api.version(ws, id, v));
    } catch (e) {
      setError(errMsg(e));
    }
  };
  const flow = open?.graph ? toFlow(open.graph) : null;

  return (
    <div className="page">
      <header className="page-head">
        <h2>Versions</h2>
        <Link to={`/workflows/${id}`}>Back to editor</Link>
      </header>
      <ErrorBanner error={error} />
      {items && items.length === 0 ? (
        <Empty title="Nothing published yet">Publish from the editor to create version 1.</Empty>
      ) : (
        <div className="split">
          <table className="table">
            <thead>
              <tr>
                <th>Version</th>
                <th>Hash</th>
                <th>Notes</th>
                <th>Published</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items?.map((v) => (
                <tr key={v.id} className={open?.version === v.version ? "active" : ""}>
                  <td>v{v.version}</td>
                  <td>
                    <code>{v.graph_hash.slice(0, 10)}</code>
                  </td>
                  <td>{v.notes}</td>
                  <td>{ago(v.created_at)}</td>
                  <td>
                    <button className="ghost" onClick={() => view(v.version)}>
                      View graph
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {flow && (
            <div className="version-canvas">
              <Canvas nodes={flow.nodes} edges={flow.edges} readOnly />
            </div>
          )}
        </div>
      )}
    </div>
  );
}
