import type {
  ApiErrorBody,
  Attempt,
  Execution,
  ExecutionDetail,
  ExecutionSummary,
  ExecEvent,
  Graph,
  Issue,
  NodeTypeInfo,
  SecretMeta,
  Session,
  WebhookInfo,
  Workflow,
  WorkflowSummary,
  WorkflowVersion,
} from "@protocol";

const TOKEN_KEY = "synapse.token";

export const tokenStore = {
  get(): string | null {
    try {
      return localStorage.getItem(TOKEN_KEY);
    } catch {
      return null;
    }
  },
  set(t: string) {
    try {
      localStorage.setItem(TOKEN_KEY, t);
    } catch {
      /* storage unavailable */
    }
  },
  clear() {
    try {
      localStorage.removeItem(TOKEN_KEY);
    } catch {
      /* storage unavailable */
    }
  },
};

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details?: unknown,
    public requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

let onUnauthorized: (() => void) | null = null;
export const setUnauthorizedHandler = (fn: (() => void) | null) => {
  onUnauthorized = fn;
};

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const h: Record<string, string> = { Accept: "application/json", ...headers };
  const token = tokenStore.get();
  if (token) h.Authorization = `Bearer ${token}`;
  if (body !== undefined) h["Content-Type"] = "application/json";
  let res: Response;
  try {
    res = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (e) {
    throw new ApiError(0, "network_error", e instanceof Error ? e.message : "network error");
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = undefined;
    }
  }
  if (!res.ok) {
    const err = (data as ApiErrorBody | undefined)?.error;
    if (res.status === 401 && token && onUnauthorized) onUnauthorized();
    throw new ApiError(res.status, err?.code ?? "error", err?.message ?? res.statusText, err?.details, err?.request_id);
  }
  return data as T;
}

const q = (params: Record<string, string | number | undefined>): string => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") p.set(k, String(v));
  const s = p.toString();
  return s ? `?${s}` : "";
};

export interface Page<T> {
  items: T[];
  next_cursor: string;
}

export interface ValidateResult {
  valid: boolean;
  issues: Issue[];
}

export const api = {
  register: (email: string, password: string, display_name: string, workspace_name?: string) =>
    request<Session>("POST", "/api/v1/auth/register", { email, password, display_name, workspace_name }),
  login: (email: string, password: string) => request<Session>("POST", "/api/v1/auth/login", { email, password }),
  logout: () => request<void>("POST", "/api/v1/auth/logout"),
  me: () => request<Session>("GET", "/api/v1/auth/me"),
  nodeTypes: () => request<{ node_types: NodeTypeInfo[] }>("GET", "/api/v1/node-types"),

  workflows: (ws: string, params: { q?: string; cursor?: string; limit?: number } = {}) =>
    request<Page<WorkflowSummary>>("GET", `/api/v1/workspaces/${ws}/workflows${q(params)}`),
  workflow: (ws: string, id: string) => request<Workflow>("GET", `/api/v1/workspaces/${ws}/workflows/${id}`),
  createWorkflow: (ws: string, name: string, graph: Graph, description = "") =>
    request<Workflow>("POST", `/api/v1/workspaces/${ws}/workflows`, { name, description, graph }),
  updateWorkflow: (ws: string, id: string, patch: { name?: string; description?: string; graph?: Graph; revision?: number }) =>
    request<Workflow>("PUT", `/api/v1/workspaces/${ws}/workflows/${id}`, patch),
  deleteWorkflow: (ws: string, id: string) => request<void>("DELETE", `/api/v1/workspaces/${ws}/workflows/${id}`),
  validate: (ws: string, id: string, graph?: Graph) =>
    request<ValidateResult>("POST", `/api/v1/workspaces/${ws}/workflows/${id}/validate`, graph ? { graph } : {}),
  publish: (ws: string, id: string, notes = "") =>
    request<WorkflowVersion>("POST", `/api/v1/workspaces/${ws}/workflows/${id}/publish`, { notes }),
  unpublish: (ws: string, id: string) => request<Workflow>("POST", `/api/v1/workspaces/${ws}/workflows/${id}/unpublish`, {}),
  versions: (ws: string, id: string) =>
    request<{ items: WorkflowVersion[] }>("GET", `/api/v1/workspaces/${ws}/workflows/${id}/versions`),
  version: (ws: string, id: string, v: number) =>
    request<WorkflowVersion>("GET", `/api/v1/workspaces/${ws}/workflows/${id}/versions/${v}`),
  webhooks: (ws: string, id: string) =>
    request<{ webhooks: WebhookInfo[] }>("GET", `/api/v1/workspaces/${ws}/workflows/${id}/webhooks`),
  run: (ws: string, id: string, trigger?: unknown, startNode?: string) =>
    request<{ execution: Execution; duplicate: boolean }>("POST", `/api/v1/workspaces/${ws}/workflows/${id}/run`, {
      trigger,
      start_node: startNode,
    }),

  executions: (ws: string, params: { workflow_id?: string; status?: string; replay_of?: string; cursor?: string; limit?: number } = {}) =>
    request<Page<ExecutionSummary>>("GET", `/api/v1/workspaces/${ws}/executions${q(params)}`),
  execution: (ws: string, id: string) => request<ExecutionDetail>("GET", `/api/v1/workspaces/${ws}/executions/${id}`),
  events: (ws: string, id: string, after = 0) =>
    request<{ events: ExecEvent[] }>("GET", `/api/v1/workspaces/${ws}/executions/${id}/events${q({ after, limit: 1000 })}`),
  cancel: (ws: string, id: string) => request<{ execution: Execution }>("POST", `/api/v1/workspaces/${ws}/executions/${id}/cancel`, {}),
  replay: (ws: string, id: string, node?: string) =>
    request<{ execution: Execution; duplicate: boolean }>(
      "POST",
      `/api/v1/workspaces/${ws}/executions/${id}/replay${node ? `/${encodeURIComponent(node)}` : ""}`,
      {},
    ),

  secrets: (ws: string) => request<{ secrets: SecretMeta[] }>("GET", `/api/v1/workspaces/${ws}/secrets`),
  putSecret: (ws: string, name: string, value: string) =>
    request<void>("PUT", `/api/v1/workspaces/${ws}/secrets/${encodeURIComponent(name)}`, { value }),
  deleteSecret: (ws: string, name: string) => request<void>("DELETE", `/api/v1/workspaces/${ws}/secrets/${encodeURIComponent(name)}`),
};

export type { Attempt };
