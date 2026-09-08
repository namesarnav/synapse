import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { useAuth } from "../state/auth";
import { Logo } from "./Logo";

export function Shell() {
  const { session, workspace, setWorkspace, logout } = useAuth();
  const nav = useNavigate();
  return (
    <div className="shell">
      <nav className="sidebar" aria-label="Main">
        <Link to="/welcome" className="brand">
          <Logo />
          Synapse
        </Link>
        {session && session.workspaces.length > 1 && (
          <select aria-label="Workspace" value={workspace?.id} onChange={(e) => setWorkspace(e.target.value)}>
            {session.workspaces.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        )}
        {session && session.workspaces.length === 1 && <div className="ws-name">{workspace?.name}</div>}
        <NavLink to="/" end>
          Dashboard
        </NavLink>
        <NavLink to="/workflows">Workflows</NavLink>
        <NavLink to="/executions">Executions</NavLink>
        <NavLink to="/settings">Settings</NavLink>
        <div className="spacer" />
        <div className="who">
          <span>{session?.user.display_name || session?.user.email}</span>
          <button
            className="ghost"
            onClick={async () => {
              await logout();
              nav("/login");
            }}
          >
            Sign out
          </button>
        </div>
      </nav>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
