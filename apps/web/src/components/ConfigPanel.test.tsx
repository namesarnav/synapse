import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { GraphNode } from "@protocol";
import { ConfigPanel } from "./ConfigPanel";

const http: GraphNode = { id: "h", type: "http_request", name: "Call", config: { method: "GET", url: "https://x" } };

function setup(node: GraphNode = http, readOnly = false) {
  const onConfig = vi.fn();
  const onPatch = vi.fn();
  render(<ConfigPanel node={node} readOnly={readOnly} onConfig={onConfig} onPatch={onPatch} onDelete={vi.fn()} />);
  return { onConfig, onPatch };
}

describe("ConfigPanel", () => {
  it("renders schema fields for the node type", () => {
    setup();
    expect(screen.getByLabelText(/URL/)).toHaveValue("https://x");
    expect(screen.getByLabelText(/Method/)).toHaveValue("GET");
    expect(screen.getByLabelText(/Headers/)).toBeInTheDocument();
  });

  it("reports config edits", async () => {
    const { onConfig } = setup();
    await userEvent.type(screen.getByLabelText(/URL/), "y");
    expect(onConfig).toHaveBeenLastCalledWith("url", "https://xy");
  });

  it("only commits JSON that parses", async () => {
    const { onConfig } = setup();
    const box = screen.getByLabelText(/Headers/);
    await userEvent.click(box);
    await userEvent.paste("{bad");
    expect(onConfig).not.toHaveBeenCalled();
    expect(screen.getByText(/Not valid JSON/)).toBeInTheDocument();
    await userEvent.clear(box);
    await userEvent.click(box);
    await userEvent.paste('{"a":"b"}');
    expect(onConfig).toHaveBeenLastCalledWith("headers", { a: "b" });
    expect(screen.queryByText(/Not valid JSON/)).not.toBeInTheDocument();
  });

  it("enables and edits the retry policy", async () => {
    const { onPatch } = setup();
    await userEvent.click(screen.getByLabelText("Retry on failure"));
    expect(onPatch).toHaveBeenCalledWith({ retry: expect.objectContaining({ max_attempts: 3 }) });
  });

  it("disables everything for viewers", () => {
    setup(http, true);
    expect(screen.getByLabelText(/URL/)).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });
});
