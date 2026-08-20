// Shared wire contracts. A Go test (tests/protocol) checks the const arrays
// below against the backend so the two sides cannot drift silently.

export const PROTOCOL_VERSION = 1;

export const NODE_TYPES = [
  "webhook_trigger",
  "schedule_trigger",
  "manual_trigger",
  "condition",
  "delay",
  "foreach",
  "transform",
  "merge",
  "stop",
  "http_request",
  "log",
  "email",
  "sub_workflow",
] as const;
export type NodeType = (typeof NODE_TYPES)[number];

export const NODE_STATES = [
  "pending",
  "ready",
  "queued",
  "running",
  "waiting",
  "succeeded",
  "failed",
  "retrying",
  "skipped",
  "cancelled",
] as const;
export type NodeState = (typeof NODE_STATES)[number];

export const EXEC_STATES = ["created", "running", "waiting", "succeeded", "failed", "cancelling", "cancelled"] as const;
export type ExecState = (typeof EXEC_STATES)[number];

export const EVENT_TYPES = [
  "execution.created",
  "execution.started",
  "execution.succeeded",
  "execution.failed",
  "execution.cancelled",
  "execution.cancelling",
  "node.queued",
  "node.started",
  "node.succeeded",
  "node.failed",
  "node.retrying",
  "node.waiting",
  "node.skipped",
  "node.cancelled",
  "node.log",
] as const;
export type EventType = (typeof EVENT_TYPES)[number];

export const MESSAGE_TYPES = ["hello", "event", "resync", "end", "error"] as const;
export type MessageType = (typeof MESSAGE_TYPES)[number];

export const ROLES = ["viewer", "member", "admin", "owner"] as const;
export type Role = (typeof ROLES)[number];

export type Category = "trigger" | "logic" | "action";

export interface NodeTypeInfo {
  type: NodeType;
  label: string;
  category: Category;
  branches?: string[];
  inline: boolean;
  terminal?: boolean;
}

export interface RetryPolicy {
  max_attempts: number;
  initial_delay_ms: number;
  max_delay_ms: number;
  multiplier: number;
  jitter: number;
}

export interface GraphNode {
  id: string;
  type: NodeType;
  name?: string;
  config?: Record<string, unknown>;
  retry?: RetryPolicy;
  timeout_ms?: number;
  on_error?: "fail" | "continue";
  metadata?: { position?: { x: number; y: number } } & Record<string, unknown>;
}

export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  branch?: string;
}

export interface Graph {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface Issue {
  severity: "error" | "warning";
  code: string;
  message: string;
  node_id?: string;
  edge_id?: string;
}

export interface User {
  id: string;
  email: string;
  display_name: string;
}

export interface Workspace {
  id: string;
  name: string;
  role?: Role;
}

export interface Session {
  user: User;
  workspaces: Workspace[];
  token?: string;
  expires_at: string;
}

export interface Workflow {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  status: string;
  graph: Graph;
  revision: number;
  published_version_id: string | null;
  published_version: number | null;
  created_at: string;
  updated_at: string;
}

export interface WorkflowSummary {
  id: string;
  name: string;
  description: string;
  status: string;
  published_version: number | null;
  node_count: number;
  updated_at: string;
}

export interface WorkflowVersion {
  id: string;
  workflow_id: string;
  version: number;
  graph?: Graph;
  graph_hash: string;
  notes: string;
  created_at: string;
}

export interface NodeError {
  code: string;
  message: string;
  retryable?: boolean;
}

export interface Execution {
  id: string;
  workspace_id: string;
  workflow_id: string;
  version_id: string;
  version: number;
  kind: string;
  status: ExecState;
  trigger_type: string;
  trigger_payload: unknown;
  start_node: string;
  output: unknown;
  error?: string;
  replay_of?: string;
  replay_source_node?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface ExecutionSummary {
  id: string;
  workflow_id: string;
  version: number;
  status: ExecState;
  trigger_type: string;
  error?: string;
  replay_of?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface NodeExec {
  node_id: string;
  node_type: string;
  state: NodeState;
  branch?: string;
  attempt: number;
  input?: unknown;
  output?: unknown;
  error?: NodeError;
  wake_at?: string;
  worker_id?: string;
  started_at?: string;
  finished_at?: string;
}

export interface Attempt {
  attempt: number;
  node_id: string;
  status: string;
  delivery_count: number;
  worker_id?: string;
  error?: NodeError;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface ExecEvent {
  id: number;
  execution_id: string;
  workflow_id?: string;
  workspace_id?: string;
  type: EventType;
  node_id?: string;
  attempt?: number;
  data?: Record<string, unknown>;
  created_at: string;
}

export interface ExecutionDetail {
  execution: Execution;
  nodes: NodeExec[];
  attempts: Attempt[];
  children: ExecutionSummary[];
}

export interface WsMessage {
  type: MessageType;
  event?: ExecEvent;
  execution_id?: string;
  status?: string;
  last_event_id?: number;
  code?: string;
  message?: string;
}

export interface ApiErrorBody {
  error: { code: string; message: string; details?: unknown; request_id?: string };
}

export interface WebhookInfo {
  id: string;
  node_id: string;
  path: string;
  signed: boolean;
}

export interface SecretMeta {
  name: string;
  created_at: string;
  updated_at: string;
}

export const isTerminalExec = (s: string): boolean => s === "succeeded" || s === "failed" || s === "cancelled";
