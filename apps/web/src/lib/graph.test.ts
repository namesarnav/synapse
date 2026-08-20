import { describe, expect, it } from "vitest";
import type { Graph } from "@protocol";
import { autoLayout, edgeId, fromFlow, toFlow } from "./graph";
import { newNode, NODE_SPECS, uniqueId } from "./schema";
import { NODE_TYPES } from "@protocol";

const g: Graph = {
  nodes: [
    { id: "t", type: "manual_trigger", metadata: { position: { x: 10, y: 20 } } },
    { id: "c", type: "condition", config: { expression: "trigger.ok" } },
    { id: "l", type: "log", config: { message: "hi" } },
  ],
  edges: [
    { id: "e1", source: "t", target: "c" },
    { id: "e2", source: "c", target: "l", branch: "true" },
  ],
};

describe("graph conversion", () => {
  it("round-trips through the flow representation", () => {
    const f = toFlow(g);
    const back = fromFlow(f.nodes, f.edges);
    expect(back.edges).toEqual(g.edges);
    expect(back.nodes.map((n) => n.id)).toEqual(["t", "c", "l"]);
    expect(back.nodes[0].metadata?.position).toEqual({ x: 10, y: 20 });
    expect(back.nodes[1].config).toEqual({ expression: "trigger.ok" });
  });
  it("maps branches to source handles", () => {
    const f = toFlow(g);
    expect(f.edges[1].sourceHandle).toBe("true");
    expect(f.edges[0].sourceHandle).toBeUndefined();
  });
  it("lays out nodes without positions by depth", () => {
    const pos = autoLayout(g);
    expect(pos.t.x).toBeLessThan(pos.c.x);
    expect(pos.c.x).toBeLessThan(pos.l.x);
  });
  it("survives cycles when laying out", () => {
    const cyc: Graph = { nodes: [{ id: "a", type: "log" }, { id: "b", type: "log" }], edges: [{ id: "1", source: "a", target: "b" }, { id: "2", source: "b", target: "a" }] };
    expect(Object.keys(autoLayout(cyc))).toHaveLength(2);
  });
  it("makes unique edge ids", () => {
    expect(edgeId("a", "b", "true", new Set())).toBe("a.true->b");
    expect(edgeId("a", "b", null, new Set(["a->b"]))).toBe("a->b#2");
  });
});

describe("node catalog", () => {
  it("covers every node type the protocol declares", () => {
    expect(NODE_SPECS.map((s) => s.type).sort()).toEqual([...NODE_TYPES].sort());
  });
  it("gives branching nodes the handles the backend expects", () => {
    expect(NODE_SPECS.find((s) => s.type === "condition")?.branches).toEqual(["true", "false"]);
    expect(NODE_SPECS.find((s) => s.type === "foreach")?.branches).toEqual(["item", "done"]);
  });
  it("creates unique ids and does not share default objects", () => {
    expect(uniqueId("log", new Set(["log_1", "log_2"]))).toBe("log_3");
    expect(uniqueId("webhook_trigger", new Set())).toBe("webhook_1");
    const a = newNode("schedule_trigger", new Set(), { x: 0, y: 0 });
    const b = newNode("schedule_trigger", new Set([a.id]), { x: 0, y: 0 });
    a.config!.cron = "changed";
    expect(b.config!.cron).toBe("*/5 * * * *");
    expect(b.id).not.toBe(a.id);
  });
});
