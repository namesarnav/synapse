import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { Session, Workspace } from "@protocol";
import { api, setUnauthorizedHandler, tokenStore } from "../lib/api";

const WS_KEY = "synapse.workspace";

interface AuthValue {
  session: Session | null;
  loading: boolean;
  workspace: Workspace | null;
  setWorkspace: (id: string) => void;
  login: (email: string, password: string) => Promise<void>;
  register: (email: string, password: string, name: string, workspace?: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const Ctx = createContext<AuthValue | null>(null);

const readWs = (): string | null => {
  try {
    return localStorage.getItem(WS_KEY);
  } catch {
    return null;
  }
};

export function AuthProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState<boolean>(() => tokenStore.get() !== null);
  const [wsId, setWsId] = useState<string | null>(readWs);

  const drop = useCallback(() => {
    tokenStore.clear();
    setSession(null);
  }, []);

  const accept = useCallback((s: Session) => {
    if (s.token) tokenStore.set(s.token);
    setSession(s);
  }, []);

  const refresh = useCallback(async () => {
    const s = await api.me();
    setSession((prev) => ({ ...s, token: prev?.token }));
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(drop);
    if (tokenStore.get() === null) return () => setUnauthorizedHandler(null);
    api
      .me()
      .then((s) => setSession(s))
      .catch(drop)
      .finally(() => setLoading(false));
    return () => setUnauthorizedHandler(null);
  }, [drop]);

  const value = useMemo<AuthValue>(() => {
    const list = session?.workspaces ?? [];
    const workspace = list.find((w) => w.id === wsId) ?? list[0] ?? null;
    return {
      session,
      loading,
      workspace,
      setWorkspace: (id) => {
        setWsId(id);
        try {
          localStorage.setItem(WS_KEY, id);
        } catch {
          /* storage unavailable */
        }
      },
      login: async (email, password) => accept(await api.login(email, password)),
      register: async (email, password, name, workspace) => accept(await api.register(email, password, name, workspace)),
      logout: async () => {
        try {
          await api.logout();
        } catch {
          /* token may already be invalid */
        }
        drop();
      },
      refresh,
    };
  }, [session, loading, wsId, accept, drop, refresh]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthValue {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth outside AuthProvider");
  return v;
}

/** The active workspace id; only valid beneath the auth gate. */
export function useWorkspaceId(): string {
  const { workspace } = useAuth();
  if (!workspace) throw new Error("no active workspace");
  return workspace.id;
}

const rank: Record<string, number> = { viewer: 0, member: 1, admin: 2, owner: 3 };
export const atLeast = (role: string | undefined, min: string): boolean => (rank[role ?? ""] ?? -1) >= rank[min];
