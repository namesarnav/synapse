import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExecEvent, ExecutionDetail as Detail, WsMessage } from "@protocol";
import { AuthProvider } from "../state/auth";
import { AppRoutes } from "../App";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const session = { user: { id: "u", email: "a@b.co", display_name: "A" }, workspaces: [{ id: "w1", name: "W", role: "member" }], expires_at: "" };

const ex = (status: string) =>
  ({ id: "11111111-aaaa", workspace_id: "w1", workflow_id: "f1", version: 2, status, trigger_type: "manual", created_at: "2024-01-01T00:00:00Z", trigger_payload: { n: 1 }, output: null }) as unknown as Detail["execution"];
const detail = (status: string): Detail => ({
  execution: ex(status),
  nodes: [
    { node_id: "t", node_type: "manual_trigger", state: "succeeded", attempt: 1, started_at: "2024-01-01T00:00:00Z", finished_at: "2024-01-01T00:00:00.1Z" },
    { node_id: "h", node_type: "http_request", state: "failed", attempt: 2, error: { code: "http_error", message: "status 503" }, worker_id: "worker-a" },
  ],
  attempts: [
    { attempt: 1, node_id: "h", status: "failed", delivery_count: 1, created_at: "", error: { code: "http_error", message: "status 503" } },
    { attempt: 2, node_id: "h", status: "failed", delivery_count: 1, created_at: "", error: { code: "http_error", message: "status 503" } },
  ],
  children: [],
});
const events: ExecEvent[] = [
  { id: 1, execution_id: "e", type: "execution.started", created_at: "2024-01-01T00:00:00Z" },
  { id: 2, execution_id: "e", type: "node.retrying", node_id: "h", created_at: "2024-01-01T00:00:01Z", data: { delay_ms: 1000, error: { code: "http_error", message: "status 503" } } },
  { id: 3, execution_id: "e", type: "node.failed", node_id: "h", created_at: "2024-01-01T00:00:02Z", data: { error: { code: "http_error", message: "status 503" } } },
];

class FakeWS {
  static last: FakeWS | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) { FakeWS.last = this; }
  close() {}
  push(m: WsMessage) { this.onmessage?.({ data: JSON.stringify(m) } as MessageEvent); }
}

describe("ExecutionDetail", () => {
  const fetchMock = vi.fn();
  const posts: string[] = [];
  beforeEach(() => {
    localStorage.setItem("synapse.token", "t");
    posts.length = 0;
    fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
      if (init?.method === "POST") {
        posts.push(url);
        return json({ execution: { id: "22222222-bbbb" }, duplicate: false }, 202);
      }
      if (url.endsWith("/auth/me")) return json(session);
      if (url.includes("/events")) return json({ events });
      if (url.includes("/versions/")) return json({ id: "v", workflow_id: "f1", version: 2, graph_hash: "h", notes: "", created_at: "", graph: { nodes: [{ id: "t", type: "manual_trigger" }, { id: "h", type: "http_request" }], edges: [{ id: "e", source: "t", target: "h" }] } });
      if (url.includes("/executions/")) return json(detail("failed"));
      if (url.includes("/workflows")) return json({ items: [], next_cursor: "" });
      return json({}, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal("WebSocket", FakeWS as unknown as typeof WebSocket);
    class RO { observe() {} unobserve() {} disconnect() {} }
    vi.stubGlobal("ResizeObserver", RO);
  });
  afterEach(() => vi.unstubAllGlobals());

  const mount = () =>
    render(
      <MemoryRouter initialEntries={["/executions/11111111-aaaa"]}>
        <AuthProvider>
          <AppRoutes />
        </AuthProvider>
      </MemoryRouter>,
    );

  it("shows the node list, timeline, error and retry history", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /Execution/ })).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: /^failed\s*h/ }));
    expect(await screen.findByText("Attempts")).toBeInTheDocument();
    expect(screen.getAllByText(/http_error/).length).toBeGreaterThan(0);
    expect(screen.getByText("worker-a")).toBeInTheDocument();
    expect(screen.getByText(/h retrying in 1\.00s: status 503/)).toBeInTheDocument();
  });

  it("offers replay for a finished execution and replays from a node", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Replay execution" }));
    await waitFor(() => expect(posts).toContain("/api/v1/workspaces/w1/executions/11111111-aaaa/replay"));
    await waitFor(() => expect(fetchMock.mock.calls.some(([u]) => String(u).endsWith("/executions/22222222-bbbb"))).toBe(true));
  });

  it("replays from the selected node", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^failed\s*h/ }));
    await userEvent.click(await screen.findByRole("button", { name: "Replay from this node" }));
    await waitFor(() => expect(posts).toContain("/api/v1/workspaces/w1/executions/11111111-aaaa/replay/h"));
  });

  it("subscribes to the stream, resuming after the loaded events", async () => {
    mount();
    await screen.findByRole("heading", { name: /Execution/ });
    await waitFor(() => expect(FakeWS.last?.url).toContain("after=3"));
    const within_ = within(document.body);
    expect(within_.getAllByText(/failed/).length).toBeGreaterThan(0);
  });
});
