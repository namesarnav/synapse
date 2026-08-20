import { beforeEach, describe, expect, it } from "vitest";
import type { Workflow } from "@protocol";
import { useEditor } from "./editor";

const wf = (over: Partial<Workflow> = {}): Workflow => ({
  id: "f1", workspace_id: "w", name: "Flow", description: "", status: "draft",
  graph: { nodes: [{ id: "t", type: "manual_trigger" }, { id: "l", type: "log", config: { message: "hi" } }], edges: [{ id: "e", source: "t", target: "l" }] },
  revision: 3, published_version_id: null, published_version: null, created_at: "", updated_at: "", ...over,
});

describe("editor store", () => {
  beforeEach(() => useEditor.getState().reset());

  it("loads a workflow clean", () => {
    useEditor.getState().load(wf());
    const s = useEditor.getState();
    expect(s.nodes).toHaveLength(2);
    expect(s.dirty).toBe(false);
    expect(s.revision).toBe(3);
  });

  it("marks dirty on edits and clears on save", () => {
    const e = useEditor.getState();
    e.load(wf());
    e.patchConfig("l", "message", "bye");
    expect(useEditor.getState().dirty).toBe(true);
    expect(useEditor.getState().graph().nodes[1].config).toEqual({ message: "bye" });
    e.saved(wf({ revision: 4 }));
    expect(useEditor.getState().dirty).toBe(false);
    expect(useEditor.getState().revision).toBe(4);
  });

  it("removes empty config values", () => {
    const e = useEditor.getState();
    e.load(wf());
    e.patchConfig("l", "message", "");
    expect(useEditor.getState().graph().nodes[1].config).toEqual({});
  });

  it("adds nodes with fresh ids and selects them", () => {
    const e = useEditor.getState();
    e.load(wf());
    const a = e.addNode("log", { x: 0, y: 0 });
    const b = e.addNode("log", { x: 0, y: 0 });
    expect(a).not.toBe(b);
    expect(useEditor.getState().selected).toBe(b);
    expect(useEditor.getState().nodes).toHaveLength(4);
  });

  it("connects with branches, and refuses self loops and duplicates", () => {
    const e = useEditor.getState();
    e.load(wf());
    const c = e.addNode("condition", { x: 0, y: 0 });
    e.connect(c, "l", "true");
    e.connect(c, "l", "true");
    e.connect(c, c, "false");
    const edges = useEditor.getState().graph().edges;
    expect(edges.filter((x) => x.source === c)).toEqual([{ id: `${c}.true->l`, source: c, target: "l", branch: "true" }]);
  });

  it("deleting a node removes its edges and clears the selection", () => {
    const e = useEditor.getState();
    e.load(wf());
    e.select("l");
    e.deleteNode("l");
    const s = useEditor.getState();
    expect(s.edges).toHaveLength(0);
    expect(s.selected).toBeNull();
    expect(s.nodes.map((n) => n.id)).toEqual(["t"]);
  });

  it("keeps positions in the serialized graph", () => {
    const e = useEditor.getState();
    e.load(wf());
    e.onNodesChange([{ id: "l", type: "position", position: { x: 123.4, y: 55.6 } }]);
    expect(useEditor.getState().graph().nodes[1].metadata?.position).toEqual({ x: 123, y: 56 });
    expect(useEditor.getState().dirty).toBe(true);
  });
});
