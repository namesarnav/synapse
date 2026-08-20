import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, setUnauthorizedHandler, tokenStore } from "./api";

const reply = (status: number, body?: unknown) => new Response(body === undefined ? null : JSON.stringify(body), { status });

describe("api client", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    localStorage.clear();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    setUnauthorizedHandler(null);
  });

  it("sends the bearer token and a JSON body", async () => {
    tokenStore.set("tok");
    fetchMock.mockResolvedValue(reply(200, { valid: true, issues: [] }));
    await api.validate("w1", "f1", { nodes: [], edges: [] });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/workspaces/w1/workflows/f1/validate");
    expect(init.method).toBe("POST");
    expect(init.headers.Authorization).toBe("Bearer tok");
    expect(init.headers["Content-Type"]).toBe("application/json");
    expect(JSON.parse(init.body)).toEqual({ graph: { nodes: [], edges: [] } });
  });

  it("omits Authorization when signed out", async () => {
    fetchMock.mockResolvedValue(reply(200, { node_types: [] }));
    await api.nodeTypes();
    expect(fetchMock.mock.calls[0][1].headers.Authorization).toBeUndefined();
  });

  it("turns the error envelope into an ApiError", async () => {
    fetchMock.mockResolvedValue(reply(409, { error: { code: "conflict", message: "workflow is not published", request_id: "r1" } }));
    const err = await api.run("w", "f").catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: "conflict", message: "workflow is not published", requestId: "r1" });
  });

  it("wraps network failures", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    await expect(api.me()).rejects.toMatchObject({ status: 0, code: "network_error" });
  });

  it("calls the unauthorized handler only for signed-in 401s", async () => {
    const h = vi.fn();
    setUnauthorizedHandler(h);
    fetchMock.mockImplementation(async () => reply(401, { error: { code: "unauthorized", message: "no" } }));
    await api.login("a@b.c", "bad").catch(() => undefined);
    expect(h).not.toHaveBeenCalled();
    tokenStore.set("stale");
    await api.me().catch(() => undefined);
    expect(h).toHaveBeenCalledTimes(1);
  });

  it("handles 204 responses and builds query strings", async () => {
    fetchMock.mockResolvedValueOnce(reply(204));
    await expect(api.deleteSecret("w", "A B")).resolves.toBeUndefined();
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/workspaces/w/secrets/A%20B");
    fetchMock.mockResolvedValueOnce(reply(200, { items: [], next_cursor: "" }));
    await api.executions("w", { status: "failed", workflow_id: "", limit: 5 });
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/workspaces/w/executions?status=failed&limit=5");
  });

  it("targets the node-specific replay route", async () => {
    fetchMock.mockImplementation(async () => reply(202, { execution: { id: "n" }, duplicate: false }));
    await api.replay("w", "e", "fetch data");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/workspaces/w/executions/e/replay/fetch%20data");
    await api.replay("w", "e");
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/workspaces/w/executions/e/replay");
  });
});
