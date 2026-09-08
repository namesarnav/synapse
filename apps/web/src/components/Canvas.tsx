import { useCallback, useMemo, type DragEvent } from "react";
import {
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Connection,
  type EdgeChange,
  type NodeChange,
} from "@xyflow/react";
import type { NodeType } from "@protocol";
import { SynapseNode, OVERLAY_KEY, type Overlay } from "./FlowNode";
import type { FlowEdge, FlowNode } from "../lib/graph";
import type { ExecView } from "../lib/execution";

const nodeTypes = { synapse: SynapseNode };

interface Props {
  nodes: FlowNode[];
  edges: FlowEdge[];
  view?: ExecView | null;
  readOnly?: boolean;
  onNodesChange?: (c: NodeChange<FlowNode>[]) => void;
  onEdgesChange?: (c: EdgeChange<FlowEdge>[]) => void;
  onConnect?: (source: string, target: string, branch?: string | null) => void;
  onDropNode?: (t: NodeType, pos: { x: number; y: number }) => void;
  onSelect?: (id: string | null) => void;
}

function Inner({ nodes, edges, view, readOnly, onNodesChange, onEdgesChange, onConnect, onDropNode, onSelect }: Props) {
  const rf = useReactFlow();

  // Fold the live run state into node data; the store itself stays free of run state.
  const shown = useMemo(
    () =>
      nodes.map((n) => {
        const nv = view?.nodes[n.id];
        const overlay: Overlay | undefined = view ? { state: nv?.state ?? "pending", attempt: nv?.attempt ?? 0, retries: nv?.retries ?? 0 } : undefined;
        return overlay ? { ...n, data: { ...n.data, [OVERLAY_KEY]: overlay } } : n;
      }),
    [nodes, view],
  );
  const animated = useMemo(
    () =>
      edges.map((e) => {
        const s = view?.nodes[e.source]?.state;
        return view ? { ...e, animated: s === "running" || s === "retrying", className: s === "succeeded" ? "edge-done" : undefined } : e;
      }),
    [edges, view],
  );

  const connect = useCallback((c: Connection) => c.source && c.target && onConnect?.(c.source, c.target, c.sourceHandle), [onConnect]);
  const drop = useCallback(
    (ev: DragEvent) => {
      ev.preventDefault();
      const t = ev.dataTransfer.getData("application/synapse-node") as NodeType;
      if (!t) return;
      onDropNode?.(t, rf.screenToFlowPosition({ x: ev.clientX, y: ev.clientY }));
    },
    [rf, onDropNode],
  );

  return (
    <div className="canvas" data-testid="canvas">
      <ReactFlow
        nodes={shown}
        edges={animated}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onConnect={connect}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        onPaneClick={() => onSelect?.(null)}
        onDrop={drop}
        onDragOver={(e) => e.preventDefault()}
        nodesDraggable={!readOnly}
        nodesConnectable={!readOnly}
        elementsSelectable
        deleteKeyCode={readOnly ? null : ["Backspace", "Delete"]}
        fitView
        fitViewOptions={{ padding: 0.3, maxZoom: 1 }}
        proOptions={{ hideAttribution: true }}
        colorMode="light"
      >
        <Background gap={20} />
        <Controls showInteractive={false} />
        <MiniMap pannable zoomable className="minimap" />
      </ReactFlow>
    </div>
  );
}

export function Canvas(p: Props) {
  return (
    <ReactFlowProvider>
      <Inner {...p} />
    </ReactFlowProvider>
  );
}
