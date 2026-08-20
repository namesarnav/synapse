import { create } from "zustand";
import { applyEdgeChanges, applyNodeChanges, type EdgeChange, type NodeChange } from "@xyflow/react";
import type { Graph, GraphNode, Issue, NodeType, Workflow } from "@protocol";
import { edgeId, fromFlow, toFlow, type FlowEdge, type FlowNode } from "../lib/graph";
import { newNode } from "../lib/schema";

interface EditorState {
  workflowId: string | null;
  name: string;
  description: string;
  revision: number;
  status: string;
  publishedVersion: number | null;
  nodes: FlowNode[];
  edges: FlowEdge[];
  selected: string | null;
  issues: Issue[];
  dirty: boolean;

  load: (w: Workflow) => void;
  reset: () => void;
  setName: (n: string) => void;
  select: (id: string | null) => void;
  onNodesChange: (c: NodeChange<FlowNode>[]) => void;
  onEdgesChange: (c: EdgeChange<FlowEdge>[]) => void;
  connect: (source: string, target: string, branch?: string | null) => void;
  addNode: (t: NodeType, pos: { x: number; y: number }) => string;
  patchNode: (id: string, patch: Partial<GraphNode>) => void;
  patchConfig: (id: string, key: string, value: unknown) => void;
  deleteNode: (id: string) => void;
  setIssues: (i: Issue[]) => void;
  saved: (w: Workflow) => void;
  graph: () => Graph;
}

const blank = {
  workflowId: null,
  name: "",
  description: "",
  revision: 0,
  status: "draft",
  publishedVersion: null,
  nodes: [] as FlowNode[],
  edges: [] as FlowEdge[],
  selected: null,
  issues: [] as Issue[],
  dirty: false,
};

const withNode = (n: FlowNode, node: GraphNode): FlowNode => ({ ...n, data: { ...n.data, node } });

export const useEditor = create<EditorState>((set, get) => ({
  ...blank,

  load: (w) => {
    const f = toFlow(w.graph);
    set({
      ...blank,
      workflowId: w.id,
      name: w.name,
      description: w.description,
      revision: w.revision,
      status: w.status,
      publishedVersion: w.published_version,
      nodes: f.nodes,
      edges: f.edges,
    });
  },
  reset: () => set({ ...blank }),
  setName: (name) => set({ name, dirty: true }),
  select: (selected) => set({ selected }),

  onNodesChange: (changes) =>
    set((s) => {
      const structural = changes.some((c) => c.type === "remove" || c.type === "add" || c.type === "position");
      const removed = changes.filter((c) => c.type === "remove").map((c) => c.id);
      return {
        nodes: applyNodeChanges(changes, s.nodes),
        selected: s.selected && removed.includes(s.selected) ? null : s.selected,
        dirty: s.dirty || structural,
      };
    }),
  onEdgesChange: (changes) =>
    set((s) => ({ edges: applyEdgeChanges(changes, s.edges), dirty: s.dirty || changes.some((c) => c.type === "remove" || c.type === "add") })),

  connect: (source, target, branch) =>
    set((s) => {
      if (source === target) return s;
      if (s.edges.some((e) => e.source === source && e.target === target && (e.sourceHandle ?? null) === (branch ?? null))) return s;
      const id = edgeId(source, target, branch, new Set(s.edges.map((e) => e.id)));
      const e: FlowEdge = { id, source, target, sourceHandle: branch || undefined, label: branch || undefined };
      return { edges: [...s.edges, e], dirty: true };
    }),

  addNode: (t, pos) => {
    const taken = new Set(get().nodes.map((n) => n.id));
    const node = newNode(t, taken, pos);
    set((s) => ({ nodes: [...s.nodes, { id: node.id, type: "synapse", position: pos, data: { node } }], selected: node.id, dirty: true }));
    return node.id;
  },

  patchNode: (id, patch) =>
    set((s) => ({ nodes: s.nodes.map((n) => (n.id === id ? withNode(n, { ...n.data.node, ...patch }) : n)), dirty: true })),

  patchConfig: (id, key, value) =>
    set((s) => ({
      nodes: s.nodes.map((n) => {
        if (n.id !== id) return n;
        const config = { ...(n.data.node.config ?? {}) };
        if (value === undefined || value === "") delete config[key];
        else config[key] = value;
        return withNode(n, { ...n.data.node, config });
      }),
      dirty: true,
    })),

  deleteNode: (id) =>
    set((s) => ({
      nodes: s.nodes.filter((n) => n.id !== id),
      edges: s.edges.filter((e) => e.source !== id && e.target !== id),
      selected: s.selected === id ? null : s.selected,
      dirty: true,
    })),

  setIssues: (issues) => set({ issues }),
  saved: (w) => set({ revision: w.revision, name: w.name, status: w.status, publishedVersion: w.published_version, dirty: false }),
  graph: () => fromFlow(get().nodes, get().edges),
}));
