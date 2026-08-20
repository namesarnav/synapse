import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AppRoutes } from "./App";
import { AuthProvider } from "./state/auth";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status });
const session = {
  user: { id: "u1", email: "a@b.co", display_name: "Ada" },
  workspaces: [{ id: "w1", name: "Acme", role: "member" }],
  token: "tok",
  expires_at: "2099-01-01T00:00:00Z",
};

function mount(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <AppRoutes />
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("app shell", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    localStorage.clear();
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    // Streams are not under test here.
    vi.stubGlobal("WebSocket", class { close() {} } as unknown as typeof WebSocket);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("sends signed-out visitors to the login form", async () => {
    mount("/workflows");
    expect(await screen.findByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows API errors on failed login", async () => {
    fetchMock.mockImplementation(async () => json(401, { error: { code: "unauthorized", message: "invalid email or password" } }));
    mount("/login");
    await userEvent.type(screen.getByLabelText("Email"), "a@b.co");
    await userEvent.type(screen.getByLabelText("Password"), "wrongpassword");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("invalid email or password");
  });

  it("logs in and lands on the dashboard", async () => {
    fetchMock.mockImplementation(async (url: string) => {
      if (url.endsWith("/auth/login")) return json(200, session);
      if (url.includes("/executions")) return json(200, { items: [], next_cursor: "" });
      if (url.includes("/workflows")) return json(200, { items: [], next_cursor: "" });
      return json(404, { error: { code: "not_found", message: "x" } });
    });
    mount("/login");
    await userEvent.type(screen.getByLabelText("Email"), "a@b.co");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse battery");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
    expect(localStorage.getItem("synapse.token")).toBe("tok");
    await waitFor(() => expect(screen.getByText("No executions yet")).toBeInTheDocument());
  });

  it("restores a session from a stored token", async () => {
    localStorage.setItem("synapse.token", "tok");
    fetchMock.mockImplementation(async (url: string) => {
      if (url.endsWith("/auth/me")) return json(200, session);
      return json(200, { items: [], next_cursor: "" });
    });
    mount("/workflows");
    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New workflow" })).toBeInTheDocument();
  });

  it("drops a rejected token and returns to login", async () => {
    localStorage.setItem("synapse.token", "stale");
    fetchMock.mockImplementation(async () => json(401, { error: { code: "unauthorized", message: "expired" } }));
    mount("/");
    expect(await screen.findByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(localStorage.getItem("synapse.token")).toBeNull();
  });

  it("hides write actions from viewers", async () => {
    localStorage.setItem("synapse.token", "tok");
    const viewer = { ...session, workspaces: [{ id: "w1", name: "Acme", role: "viewer" }] };
    fetchMock.mockImplementation(async (url: string) => {
      if (url.endsWith("/auth/me")) return json(200, viewer);
      return json(200, { items: [{ id: "f1", name: "Flow", description: "", status: "draft", published_version: null, node_count: 1, updated_at: new Date().toISOString() }], next_cursor: "" });
    });
    mount("/workflows");
    expect(await screen.findByText("Flow")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New workflow" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });
});
