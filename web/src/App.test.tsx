// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { ApprovalItem } from "@/lib/api";
import { installFakeApi } from "@/test/fakeApi";
import App from "./App";

const pending = (id: string): ApprovalItem => ({
  id,
  run_id: "r1",
  run_node_id: "n1",
  kind: "node",
  node_id: "gate",
  message: "Deploy?",
  status: "pending",
  requested_at: "2026-10-15T10:00:00Z",
  expires_at: "2026-10-16T10:00:00Z",
});

let waiting: ApprovalItem[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  waiting = [pending("a1"), pending("a2")];
  api = installFakeApi();
  api.on("GET", "/api/workflows", () => []);
  api.on("GET", /^\/api\/approvals\?status=pending$/, () => waiting);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderApp(path = "/workflows") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("navigation", () => {
  it("links to every page, Workflows and Approvals included", async () => {
    renderApp();
    const nav = within(screen.getByRole("navigation"));
    expect(nav.getAllByRole("link").map((l) => l.getAttribute("href"))).toEqual(["/agents", "/workflows", "/runs", "/approvals", "/status"]);
    expect(await screen.findByRole("heading", { name: "Workflows" })).not.toBeNull();
  });

  it("badges Approvals with the number of requests waiting and keeps it current", async () => {
    renderApp();
    const link = screen.getByRole("link", { name: /^Approvals/ });
    expect((await within(link).findByLabelText("2 pending")).textContent).toBe("2");

    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    waiting = [pending("a2")];
    api.send("/api/events", "approval.decided", { approval: { ...pending("a1"), status: "approved" } });
    await waitFor(() => expect(within(link).getByLabelText("1 pending")).not.toBeNull());

    waiting = [pending("a2"), pending("a3"), pending("a4")];
    api.send("/api/events", "approval.requested", { approval: pending("a3") });
    await waitFor(() => expect(within(link).getByLabelText("3 pending")).not.toBeNull());
  });

  it("shows no badge when nothing is waiting", async () => {
    waiting = [];
    renderApp();
    await waitFor(() => expect(api.made("GET", /approvals/).length).toBeGreaterThan(0));
    expect(within(screen.getByRole("link", { name: "Approvals" })).queryByLabelText(/pending/)).toBeNull();
  });
});
