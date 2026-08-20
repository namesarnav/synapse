import { memo } from "react";
import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { NodeState } from "@protocol";
import { specFor } from "../lib/schema";
import type { FlowNode as FN } from "../lib/graph";
import { STATE_GLYPH } from "../lib/execution";

export interface Overlay {
  state: NodeState;
  attempt: number;
  retries: number;
}

// Overlay is passed through node data under this key when a run is being shown.
export const OVERLAY_KEY = "overlay";

function SynapseNodeImpl({ data, selected }: NodeProps<FN>) {
  const node = data.node;
  const spec = specFor(node.type);
  const overlay = data[OVERLAY_KEY] as Overlay | undefined;
  const isTrigger = spec?.category === "trigger";
  const branches = spec?.branches;
  return (
    <div className={`fnode cat-${spec?.category ?? "action"} ${selected ? "is-selected" : ""} ${overlay ? `run-${overlay.state}` : ""}`} data-testid={`node-${node.id}`}>
      {!isTrigger && <Handle type="target" position={Position.Left} />}
      <div className="fnode-head">
        <span className="fnode-type">{spec?.label ?? node.type}</span>
        {overlay && (
          <span className={`fnode-state st-${overlay.state}`} title={`${overlay.state}${overlay.retries ? `, ${overlay.retries} retries` : ""}`}>
            <span aria-hidden="true">{STATE_GLYPH[overlay.state]}</span> {overlay.state}
            {overlay.retries > 0 && <span className="retries"> ×{overlay.retries + 1}</span>}
          </span>
        )}
      </div>
      <div className="fnode-name">{node.name || node.id}</div>
      <div className="fnode-id">{node.id}</div>
      {branches ? (
        branches.map((b, i) => (
          <Handle key={b} id={b} type="source" position={Position.Right} style={{ top: `${((i + 1) * 100) / (branches.length + 1)}%` }}>
            <span className="handle-label">{b}</span>
          </Handle>
        ))
      ) : (
        <Handle type="source" position={Position.Right} />
      )}
    </div>
  );
}

export const SynapseNode = memo(SynapseNodeImpl);
