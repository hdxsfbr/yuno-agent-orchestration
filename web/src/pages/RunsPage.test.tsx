// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useParams } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Run } from "@/lib/api";
import RunsPage from "./RunsPage";

const DONE = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";
const QUEUED = "01a14162-0000-7000-8000-000000000001";

const base = (over: Partial<Run>): Run => ({
  id: DONE,
  workflow_id: "wf",
  workflow_version: 1,
  trigger: "manual",
  status: "succeeded",
  input: {},
  state: {},
  limits: {},
  step_count: 6,
  tool_call_count: 6,
  cost_usd_micros: 62_064,
  created_at: "2026-10-15T21:04:53Z",
  started_at: "2026-10-15T21:04:53Z",
  finished_at: "2026-10-15T21:05:18Z",
  ...over,
});

const runs: Run[] = [
  base({ agent_names: ["Coder"], models: ["claude-sonnet-5-5"] }),
  base({ id: QUEUED, status: "queued", step_count: 0, tool_call_count: 0, cost_usd_micros: 0, started_at: undefined, finished_at: undefined }),
];

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url.startsWith("/api/runs?")) return json(runs);
      if (url === "/api/events") {
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            c.enqueue(new TextEncoder().encode(": connected\n\n"));
            init?.signal?.addEventListener("abort", () => {
              try {
                c.close();
              } catch {
                /* already closed */
              }
            });
          },
        });
        return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
      }
      return json({ error: { code: "not_found", message: "not found" } }, 404);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function RunMarker() {
  const { id } = useParams();
  return <p>run page {id}</p>;
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/runs"]}>
        <Routes>
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunMarker />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunsPage", () => {
  it("lists each run with its agent and model, or a dash before it has either", async () => {
    renderPage();
    const link = await screen.findByTitle(DONE);
    const row = link.closest("tr")!;
    expect(screen.getByRole("columnheader", { name: "Agent" })).not.toBeNull();
    expect(screen.getByRole("columnheader", { name: "Model" })).not.toBeNull();
    expect(within(row).getByText("Coder")).not.toBeNull();
    expect(within(row).getByText("claude-sonnet-5-5")).not.toBeNull();

    const queued = within(screen.getByTitle(QUEUED).closest("tr")!).getAllByRole("cell");
    expect(queued[1].textContent).toBe("—");
    expect(queued[2].textContent).toBe("—");
  });

  it("opens the run when any cell of its row is clicked", async () => {
    renderPage();
    const row = (await screen.findByTitle(DONE)).closest("tr")!;
    fireEvent.click(within(row).getByText("manual"));
    expect(await screen.findByText(`run page ${DONE}`)).not.toBeNull();
  });
});
