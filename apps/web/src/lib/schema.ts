import type { Category, GraphNode, NodeType, RetryPolicy } from "@protocol";

export type FieldKind = "text" | "expr" | "textarea" | "number" | "select" | "json" | "bool";

export interface Field {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: string[];
  required?: boolean;
}

export interface NodeSpec {
  type: NodeType;
  label: string;
  category: Category;
  blurb: string;
  branches?: string[];
  fields: Field[];
  defaults: Record<string, unknown>;
}

export const NODE_SPECS: NodeSpec[] = [
  {
    type: "webhook_trigger",
    label: "Webhook",
    category: "trigger",
    blurb: "Start when an HTTP request arrives",
    fields: [{ key: "hmac_secret", label: "HMAC secret name", kind: "text", placeholder: "WEBHOOK_SECRET", help: "Name of a workspace secret. When set, requests must carry a valid X-Synapse-Signature." }],
    defaults: {},
  },
  {
    type: "schedule_trigger",
    label: "Schedule",
    category: "trigger",
    blurb: "Start on a cron schedule",
    fields: [
      { key: "cron", label: "Cron", kind: "text", placeholder: "*/5 * * * *", required: true },
      { key: "timezone", label: "Timezone", kind: "text", placeholder: "UTC" },
      { key: "payload", label: "Payload", kind: "json", help: "Delivered as trigger data." },
    ],
    defaults: { cron: "*/5 * * * *", timezone: "UTC" },
  },
  {
    type: "manual_trigger",
    label: "Manual",
    category: "trigger",
    blurb: "Start by hand from the editor",
    fields: [{ key: "sample", label: "Sample payload", kind: "json", help: "Pre-filled when running from the editor." }],
    defaults: {},
  },
  {
    type: "condition",
    label: "Condition",
    category: "logic",
    blurb: "Branch on an expression",
    branches: ["true", "false"],
    fields: [{ key: "expression", label: "Expression", kind: "expr", placeholder: "trigger.amount > 100", required: true }],
    defaults: { expression: "" },
  },
  {
    type: "delay",
    label: "Delay",
    category: "logic",
    blurb: "Wait, durably, before continuing",
    fields: [
      { key: "duration", label: "Duration", kind: "text", placeholder: "30s, 5m, 2h" },
      { key: "duration_ms", label: "Duration (ms)", kind: "number" },
      { key: "until", label: "Until", kind: "expr", help: "Expression returning an RFC 3339 time." },
    ],
    defaults: { duration: "10s" },
  },
  {
    type: "foreach",
    label: "For each",
    category: "logic",
    blurb: "Run the item branch once per element",
    branches: ["item", "done"],
    fields: [
      { key: "items", label: "Items", kind: "expr", placeholder: "trigger.orders", required: true },
      { key: "concurrency", label: "Concurrency", kind: "number", placeholder: "1" },
      { key: "max_items", label: "Max items", kind: "number" },
      { key: "on_item_error", label: "On item error", kind: "select", options: ["fail", "continue"] },
    ],
    defaults: { items: "", concurrency: 1 },
  },
  {
    type: "transform",
    label: "Transform",
    category: "logic",
    blurb: "Compute a value with expressions",
    fields: [
      { key: "expression", label: "Expression", kind: "expr", placeholder: "trigger.first + ' ' + trigger.last" },
      { key: "output", label: "Output template", kind: "json", help: "Object whose string values are expressions in {{ }}." },
    ],
    defaults: { expression: "" },
  },
  {
    type: "merge",
    label: "Merge",
    category: "logic",
    blurb: "Wait for parallel branches to join",
    fields: [],
    defaults: {},
  },
  {
    type: "stop",
    label: "Stop",
    category: "logic",
    blurb: "End the execution",
    fields: [
      { key: "status", label: "Final status", kind: "select", options: ["succeeded", "failed"] },
      { key: "message", label: "Message", kind: "text" },
    ],
    defaults: { status: "succeeded" },
  },
  {
    type: "http_request",
    label: "HTTP request",
    category: "action",
    blurb: "Call an HTTP endpoint",
    fields: [
      { key: "method", label: "Method", kind: "select", options: ["GET", "POST", "PUT", "PATCH", "DELETE"], required: true },
      { key: "url", label: "URL", kind: "expr", placeholder: "https://api.example.com/items", required: true },
      { key: "headers", label: "Headers", kind: "json", help: 'e.g. {"Authorization": "Bearer {{secrets.API_TOKEN}}"}' },
      { key: "query", label: "Query", kind: "json" },
      { key: "body", label: "Body", kind: "json" },
      { key: "expect_status", label: "Expected status", kind: "json", help: "A list of accepted status codes; default is any 2xx." },
      { key: "idempotency_header", label: "Idempotency header", kind: "text", placeholder: "Idempotency-Key" },
      { key: "follow_redirects", label: "Follow redirects", kind: "bool" },
    ],
    defaults: { method: "GET", url: "" },
  },
  {
    type: "log",
    label: "Log",
    category: "action",
    blurb: "Write a line to the execution log",
    fields: [
      { key: "message", label: "Message", kind: "expr", required: true },
      { key: "level", label: "Level", kind: "select", options: ["debug", "info", "warn", "error"] },
      { key: "fields", label: "Fields", kind: "json" },
    ],
    defaults: { message: "", level: "info" },
  },
  {
    type: "email",
    label: "Email",
    category: "action",
    blurb: "Send an email (simulated in dev)",
    fields: [
      { key: "to", label: "To", kind: "expr", required: true },
      { key: "from", label: "From", kind: "text" },
      { key: "subject", label: "Subject", kind: "expr", required: true },
      { key: "body", label: "Body", kind: "textarea" },
    ],
    defaults: { to: "", subject: "", body: "" },
  },
  {
    type: "sub_workflow",
    label: "Sub-workflow",
    category: "action",
    blurb: "Run another published workflow",
    fields: [
      { key: "workflow_id", label: "Workflow ID", kind: "text", required: true },
      { key: "input", label: "Input", kind: "json" },
    ],
    defaults: { workflow_id: "" },
  },
];

export const specFor = (t: string): NodeSpec | undefined => NODE_SPECS.find((s) => s.type === t);

export const CATEGORIES: { id: Category; title: string }[] = [
  { id: "trigger", title: "Triggers" },
  { id: "logic", title: "Logic" },
  { id: "action", title: "Actions" },
];

export const DEFAULT_RETRY: RetryPolicy = { max_attempts: 3, initial_delay_ms: 1000, max_delay_ms: 60000, multiplier: 2, jitter: 0.2 };

/** Returns a node id unused in `taken`, based on the type name. */
export function uniqueId(type: string, taken: Set<string>): string {
  const base = type.replace(/_trigger$/, "");
  let n = 1;
  while (taken.has(`${base}_${n}`)) n++;
  return `${base}_${n}`;
}

export function newNode(type: NodeType, taken: Set<string>, position: { x: number; y: number }): GraphNode {
  const spec = specFor(type);
  return {
    id: uniqueId(type, taken),
    type,
    name: spec?.label ?? type,
    config: structuredClone(spec?.defaults ?? {}),
    metadata: { position },
  };
}
