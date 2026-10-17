// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useParams } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Run } from "@/lib/api";
import { installFakeApi } from "@/test/fakeApi";
import RunsPage from "./RunsPage";

const DONE = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";
const QUEUED = "01a14162-0000-7000-8000-000000000001";
const DELEGATED = "01a14163-0000-7000-8000-000000000002";
const DELEGATED_BARE = "01a14164-0000-7000-8000-000000000003";
const OTHER_REF = "01a14165-0000-7000-8000-000000000004";
const ODD_ID = "01a14166-0000-7000-8000-000000000005";

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
  base({ agent_names: ["Coder"], models: ["claude-sonnet-5-5"], toolchain: "go" }),
  base({ id: QUEUED, status: "queued", step_count: 0, tool_call_count: 0, cost_usd_micros: 0, started_at: undefined, finished_at: undefined }),
  base({ id: DELEGATED, trigger: "channel", trigger_ref: "conversation:c1", agent_names: ["Reviewer"] }),
  base({ id: DELEGATED_BARE, trigger: "channel", trigger_ref: "" }),
  base({ id: OTHER_REF, trigger: "channel", trigger_ref: "telegram:1001" }),
  base({ id: ODD_ID, trigger: "channel", trigger_ref: "conversation:a/b c" }),
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

function ConversationMarker() {
  const { conversationId } = useParams();
  return <p>conversation page {conversationId}</p>;
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/runs"]}>
        <Routes>
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunMarker />} />
          <Route path="/channels/:conversationId" element={<ConversationMarker />} />
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

  it("shows the toolchain of a project run next to its status, and nothing for the others", async () => {
    renderPage();
    const row = (await screen.findByTitle(DONE)).closest("tr")!;
    expect(within(row).getByText("go").getAttribute("title")).toContain("Toolchain");
    const queued = screen.getByTitle(QUEUED).closest("tr")!;
    expect(within(queued).queryByTitle(/Toolchain/)).toBeNull();
  });

  it("opens the run when any cell of its row is clicked", async () => {
    renderPage();
    const row = (await screen.findByTitle(DONE)).closest("tr")!;
    fireEvent.click(within(row).getByText("manual"));
    expect(await screen.findByText(`run page ${DONE}`)).not.toBeNull();
  });
});

describe("RunsPage delegated runs", () => {
  const triggerCell = async (id: string) => within((await screen.findByTitle(id)).closest("tr")!).getAllByRole("cell")[4];

  it("shows a run an agent delegated from a conversation as Delegated, with a link back to that conversation", async () => {
    renderPage();
    const cell = within(await triggerCell(DELEGATED));
    expect(cell.getByText("Delegated")).not.toBeNull();
    expect(cell.queryByText("channel")).toBeNull();
    expect(cell.getByRole("link", { name: "Conversation" }).getAttribute("href")).toBe("/channels/c1");
  });

  it("opens the conversation, not the run, when its link is clicked", async () => {
    renderPage();
    fireEvent.click(within(await triggerCell(DELEGATED)).getByRole("link", { name: "Conversation" }));
    expect(await screen.findByText("conversation page c1")).not.toBeNull();
    expect(screen.queryByText(`run page ${DELEGATED}`)).toBeNull();
  });

  it("still opens the run from the rest of the row", async () => {
    renderPage();
    const row = (await screen.findByTitle(DELEGATED)).closest("tr")!;
    fireEvent.click(within(row).getByText("Delegated"));
    expect(await screen.findByText(`run page ${DELEGATED}`)).not.toBeNull();
  });

  it("shows Delegated without a link when the run has no conversation", async () => {
    renderPage();
    const cell = within(await triggerCell(DELEGATED_BARE));
    expect(cell.getByText("Delegated")).not.toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });

  it("shows a channel run whose reference names no conversation as the trigger with its reference, not as delegated", async () => {
    renderPage();
    const cell = within(await triggerCell(OTHER_REF));
    expect(cell.getByText("channel")).not.toBeNull();
    const ref = cell.getByText("telegram:1001");
    expect(ref.className).toContain("font-mono");
    expect(cell.queryByText("Delegated")).toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });

  it("encodes the conversation id in the link", async () => {
    renderPage();
    const cell = within(await triggerCell(ODD_ID));
    expect(cell.getByRole("link", { name: "Conversation" }).getAttribute("href")).toBe("/channels/a%2Fb%20c");
  });

  it("leaves other triggers as they are", async () => {
    renderPage();
    const cell = within(await triggerCell(DONE));
    expect(cell.getByText("manual")).not.toBeNull();
    expect(cell.queryByText("Delegated")).toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });
});

describe("RunsPage live list", () => {
  it("refreshes the list on any event, once for a burst", async () => {
    const api = installFakeApi();
    api.on("GET", /^\/api\/runs\?/, () => runs);
    renderPage();
    await screen.findByTitle(DONE);
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));

    for (const event of ["log", "tool_call", "model_call", "agent_message"]) api.send("/api/events", event, {});
    await waitFor(() => expect(api.made("GET", /^\/api\/runs\?/)).toHaveLength(2));
    await act(async () => void (await new Promise((r) => setTimeout(r, 800))));
    expect(api.made("GET", /^\/api\/runs\?/)).toHaveLength(2);
  });
});
