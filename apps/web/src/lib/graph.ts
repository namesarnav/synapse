import type { Edge, Node } from "@xyflow/react";
import type { Graph, GraphEdge, GraphNode } from "@protocol";

export type SynapseNodeData = { node: GraphNode } & Record<string, unknown>;
export type FlowNode = Node<SynapseNodeData, "synapse">;
export type FlowEdge = Edge;

const GRID_X = 260;
const GRID_Y = 130;

/** Lays out nodes that have no stored position in columns by depth. */
export function autoLayout(g: Graph): Record<string, { x: number; y: number }> {
  const incoming = new Map<string, string[]>();
  for (const n of g.nodes) incoming.set(n.id, []);
  for (const e of g.edges) incoming.get(e.target)?.push(e.source);
  const depth = new Map<string, number>();
  const visit = (id: string, stack: Set<string>): number => {
    const known = depth.get(id);
    if (known !== undefined) return known;
    if (stack.has(id)) return 0;
    stack.add(id);
    let d = 0;
    for (const s of incoming.get(id) ?? []) d = Math.max(d, visit(s, stack) + 1);
    stack.delete(id);
    depth.set(id, d);
    return d;
  };
  const perColumn = new Map<number, number>();
  const out: Record<string, { x: number; y: number }> = {};
  for (const n of g.nodes) {
    const d = visit(n.id, new Set());
    const row = perColumn.get(d) ?? 0;
    perColumn.set(d, row + 1);
    out[n.id] = { x: 40 + d * GRID_X, y: 40 + row * GRID_Y };
  }
  return out;
}

export function toFlow(g: Graph): { nodes: FlowNode[]; edges: FlowEdge[] } {
  const layout = autoLayout(g);
  const nodes: FlowNode[] = g.nodes.map((n) => ({
    id: n.id,
    type: "synapse",
    position: n.metadata?.position ?? layout[n.id],
    data: { node: n },
  }));
  const edges: FlowEdge[] = g.edges.map((e) => ({
    id: e.id,
    source: e.source,
    target: e.target,
    sourceHandle: e.branch || undefined,
    label: e.branch || undefined,
  }));
  return { nodes, edges };
}

export function fromFlow(nodes: FlowNode[], edges: FlowEdge[]): Graph {
  const gn: GraphNode[] = nodes.map((n) => ({
    ...n.data.node,
    metadata: { ...n.data.node.metadata, position: { x: Math.round(n.position.x), y: Math.round(n.position.y) } },
  }));
  const ge: GraphEdge[] = edges.map((e) => {
    const out: GraphEdge = { id: e.id, source: e.source, target: e.target };
    if (e.sourceHandle) out.branch = e.sourceHandle;
    return out;
  });
  return { nodes: gn, edges: ge };
}

export function edgeId(source: string, target: string, branch: string | null | undefined, taken: Set<string>): string {
  const base = `${source}${branch ? `.${branch}` : ""}->${target}`;
  let id = base;
  let n = 2;
  while (taken.has(id)) id = `${base}#${n++}`;
  return id;
}
