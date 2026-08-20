import { useEffect, useState, type FormEvent } from "react";
import type { SecretMeta } from "@protocol";
import { api } from "../lib/api";
import { atLeast, useAuth, useWorkspaceId } from "../state/auth";
import { Empty, ErrorBanner, ago, errMsg } from "../components/ui";

const NAME_RE = /^[A-Za-z_][A-Za-z0-9_]{0,63}$/;

export function Settings() {
  const ws = useWorkspaceId();
  const { workspace, session } = useAuth();
  const isAdmin = atLeast(workspace?.role, "admin");
  const canList = atLeast(workspace?.role, "member");
  const [items, setItems] = useState<SecretMeta[] | null>(null);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);

  const load = () =>
    api
      .secrets(ws)
      .then((r) => setItems(r.secrets))
      .catch((e) => setError(errMsg(e)));
  useEffect(() => {
    if (canList) void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ws, canList]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!NAME_RE.test(name)) return setError("Names use letters, digits and underscores, and cannot start with a digit.");
    try {
      await api.putSecret(ws, name, value);
      setName("");
      setValue("");
      await load();
    } catch (err) {
      setError(errMsg(err));
    }
  };
  const remove = async (n: string) => {
    if (!window.confirm(`Delete secret ${n}? Workflows that use it will fail.`)) return;
    try {
      await api.deleteSecret(ws, n);
      await load();
    } catch (err) {
      setError(errMsg(err));
    }
  };

  return (
    <div className="page narrow">
      <header className="page-head">
        <h2>Settings</h2>
      </header>
      <section>
        <h3>Workspace</h3>
        <dl>
          <dt>Name</dt>
          <dd>{workspace?.name}</dd>
          <dt>Your role</dt>
          <dd>{workspace?.role}</dd>
          <dt>Signed in as</dt>
          <dd>{session?.user.email}</dd>
        </dl>
      </section>
      <section>
        <h3>Secrets</h3>
        <p className="muted">
          Reference a secret as <code>{"{{secrets.NAME}}"}</code>. Values are encrypted at rest, never shown again, and masked in logs and execution records.
        </p>
        <ErrorBanner error={error} />
        {isAdmin && (
          <form className="secret-form" onSubmit={save}>
            <input aria-label="Secret name" placeholder="NAME" value={name} onChange={(e) => setName(e.target.value)} />
            <input aria-label="Secret value" type="password" placeholder="value" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="off" />
            <button className="primary" type="submit" disabled={!name || !value}>
              Save secret
            </button>
          </form>
        )}
        {!isAdmin && <p className="muted">Only admins can change secrets.</p>}
        {items && items.length === 0 ? (
          <Empty title="No secrets" />
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Updated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items?.map((s) => (
                <tr key={s.name}>
                  <td>
                    <code>{s.name}</code>
                  </td>
                  <td>{ago(s.updated_at)}</td>
                  <td className="actions">
                    {isAdmin && (
                      <button className="ghost danger" onClick={() => remove(s.name)}>
                        Delete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
