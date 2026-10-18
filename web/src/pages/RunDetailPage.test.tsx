// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { MergeRequest, ModelCall, Run, RunNode, ToolCall, Trace } from "@/lib/api";
import { mergeRequest } from "@/test/fixtures";
import RunDetailPage from "./RunDetailPage";

// A fake API: GET /api/runs/r1/trace returns `trace`, GET /api/runs/r1/events is an event
// stream the test writes to, POST /api/runs/r1/cancel answers 202.

const RUN = "r1";
const run = (over: Partial<Run> = {}): Run => ({
  id: RUN,
  workflow_id: "wf",
  workflow_version: 1,
  trigger: "manual",
  status: "running",
  input: {},
  state: {},
  limits: {},
  step_count: 1,
  tool_call_count: 0,
  cost_usd_micros: 12_345,
  input_tokens: 1200,
  output_tokens: 300,
  cache_read_tokens: 800,
  cache_write_tokens: 50,
  created_at: "2026-10-15T10:00:00Z",
  started_at: "2026-10-15T10:00:01Z",
  ...over,
});
const node: RunNode = { id: "n1", run_id: RUN, node_id: "agent", visit: 1, iteration: 1, status: "running", started_at: "2026-10-15T10:00:01Z" };
const modelCall: ModelCall = {
  id: "m1",
  run_id: RUN,
  run_node_id: "n1",
  provider: "anthropic",
  model: "claude-opus-5-5",
  stop_reason: "tool_use",
  input_tokens: 1200,
  output_tokens: 300,
  cache_read_tokens: 800,
  cache_write_tokens: 0,
  cost_usd_micros: 12_345,
  duration_ms: 2300,
  attempt: 1,
  started_at: "2026-10-15T10:00:02Z",
};
const toolCall: ToolCall = {
  id: "t1",
  run_id: RUN,
  run_node_id: "n1",
  tool_use_id: "toolu_1",
  tool_name: "read",
  input: { path: "notes.txt" },
  output: "file contents",
  is_error: false,
  status: "executed",
  attempt: 1,
  duration_ms: 15,
  started_at: "2026-10-15T10:00:03Z",
};

let trace: Trace;
/** What GET /api/merge-requests?run_id=r1 answers, and what a sync of the record returns. */
let requestsForRun: MergeRequest[];
let synced: MergeRequest | undefined;
let streamController: ReadableStreamDefaultController<Uint8Array>;
let calls: { url: string; init?: RequestInit }[];
const encoder = new TextEncoder();

function send(id: number, event: string, data: unknown) {
  streamController.enqueue(encoder.encode(`id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

beforeEach(() => {
  calls = [];
  requestsForRun = [];
  synced = undefined;
  trace = { run: run(), nodes: [node], model_calls: [modelCall], tool_calls: [], spans: [], agent_messages: [], approvals: [] };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      if (url === `/api/runs/${RUN}/trace`) return json(trace);
      if (url === `/api/runs/${RUN}/events`) {
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            streamController = c;
            c.enqueue(encoder.encode(": connected\n\n"));
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
      if (url === `/api/runs/${RUN}/cancel` && init?.method === "POST") return json(run({ cancel_requested: true }), 202);
      if (url.startsWith("/api/merge-requests?")) return json(requestsForRun);
      if (/^\/api\/merge-requests\/[^/]+\/sync$/.test(url) && init?.method === "POST") return json(synced ?? requestsForRun[0] ?? trace.run.merge_request);
      return json({ error: { code: "not_found", message: "not found" } }, 404);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/runs/${RUN}`]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunDetailPage", () => {
  it("shows the trace once the stream is open, then follows the stream", async () => {
    renderPage();

    expect(await screen.findByRole("heading", { name: /Run/ })).not.toBeNull();
    expect(await screen.findByText("Live")).not.toBeNull();

    // header
    const header = screen.getByRole("region", { name: "Run summary" });
    expect(within(header).getByText("running")).not.toBeNull();
    expect(within(header).getByText("$0.0123")).not.toBeNull();
    expect(within(header).getByText("1,200")).not.toBeNull();
    expect(within(header).getByText("800 / 50")).not.toBeNull();

    // node card with its model call
    const card = screen.getByRole("region", { name: "Node agent" });
    expect(within(card).getByText("claude-opus-5-5")).not.toBeNull();
    expect(within(card).getByText("tool_use")).not.toBeNull();
    expect(within(card).getByText("800 / 0")).not.toBeNull();
    expect(within(card).getByText("1 turn")).not.toBeNull();

    // the stream was opened with a replay from the start
    const streamCall = calls.find((c) => c.url.endsWith("/events"))!;
    expect((streamCall.init?.headers as Record<string, string>)["Last-Event-ID"]).toBe("1");

    // live events
    send(7, "tool_call", { tool_call: toolCall });
    expect(await within(card).findByText("read")).not.toBeNull();
    send(8, "node.status", { node: { ...node, status: "succeeded", finished_at: "2026-10-15T10:00:09Z", output: { answer: 42 } } });
    await waitFor(() => expect(within(card).getByText("succeeded")).not.toBeNull());
    expect(card.textContent).toContain('"answer": 42');
    send(9, "log", { run_id: RUN, level: "warn", message: "tool was slow" });
    expect(await screen.findByText("tool was slow")).not.toBeNull();
    send(10, "run.status", { run: run({ status: "succeeded", finished_at: "2026-10-15T10:00:09Z", step_count: 2 }) });
    await waitFor(() => expect(screen.queryByRole("button", { name: /Cancel/ })).toBeNull());
  });

  it("updates the run summary from the counters live call events carry", async () => {
    renderPage();
    const header = await screen.findByRole("region", { name: "Run summary" });
    await screen.findByText("Live");
    send(7, "model_call", { model_call: { ...modelCall, id: "m2" }, run: run({ step_count: 2, input_tokens: 2_500, cost_usd_micros: 54_321 }) });
    await waitFor(() => expect(within(header).getByText("$0.0543")).not.toBeNull());
    expect(within(header).getByText("2,500")).not.toBeNull();
  });

  it("reloads the trace when the run finishes while the trace is still loading", async () => {
    let release: () => void = () => undefined;
    let traceLoads = 0;
    const fetchMock = fetch as unknown as ReturnType<typeof vi.fn>;
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === `/api/runs/${RUN}/trace` && ++traceLoads === 1) {
        // The first load is slow: the run finishes while it is in flight.
        const body = trace;
        await new Promise<void>((r) => (release = r));
        calls.push({ url, init });
        return json(body);
      }
      return base(url, init);
    });
    renderPage();
    await waitFor(() => expect(traceLoads).toBe(1));
    send(5, "run.status", { run: run({ status: "succeeded", finished_at: "2026-10-15T10:00:09Z", step_count: 2 }) });
    trace = { ...trace, run: run({ status: "succeeded", finished_at: "2026-10-15T10:00:09Z", step_count: 2 }), spans: [] };
    await new Promise((r) => setTimeout(r, 20));
    release();
    await waitFor(() => expect(traceLoads).toBe(2), { timeout: 3000 });
    await waitFor(() => expect(screen.queryByRole("button", { name: /Cancel/ })).toBeNull());
  });

  it("cancels a running run and disables the button", async () => {
    renderPage();
    const cancel = await screen.findByRole("button", { name: "Cancel run" });
    fireEvent.click(cancel);
    await waitFor(() => expect((screen.getByRole("button", { name: "Cancel requested" }) as HTMLButtonElement).disabled).toBe(true));
    expect(calls.some((c) => c.url === `/api/runs/${RUN}/cancel` && c.init?.method === "POST")).toBe(true);
  });

  it("shows an error with a retry when the trace cannot be loaded", async () => {
    trace = undefined as unknown as Trace;
    (fetch as unknown as ReturnType<typeof vi.fn>).mockImplementation(async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      if (url.endsWith("/events")) return new Response(new ReadableStream<Uint8Array>({ start(c) { streamController = c; } }), { status: 200 });
      return json({ error: { code: "not_found", message: "run not found" } }, 404);
    });
    renderPage();
    expect(await screen.findByText(/run not found/)).not.toBeNull();
    expect(screen.getByRole("button", { name: "Retry" })).not.toBeNull();
  });
});

describe("RunDetailPage trigger", () => {
  const trigger = async () => {
    const header = within(await screen.findByRole("region", { name: "Run summary" }));
    return within(header.getByText("Trigger").closest("div")!);
  };

  it("says a run was delegated from a conversation and links to it", async () => {
    trace = { ...trace, run: run({ trigger: "channel", trigger_ref: "conversation:c1" }) };
    renderPage();
    const value = await trigger();
    expect(value.getByRole("link", { name: "Delegated from a conversation" }).getAttribute("href")).toBe("/channels/c1");
    expect(value.queryByText("conversation:c1")).toBeNull();
    expect(within(screen.getByRole("region", { name: "Run summary" })).getByRole("heading", { level: 1 }).parentElement!.textContent).toContain("Delegated");
  });

  it("says Delegated, without a link, when the run has no conversation", async () => {
    trace = { ...trace, run: run({ trigger: "channel", trigger_ref: "" }) };
    renderPage();
    const value = await trigger();
    expect(value.getByText("Delegated")).not.toBeNull();
    expect(value.queryByRole("link")).toBeNull();
  });

  it("shows a channel trigger whose reference names no conversation as it is, not as delegated", async () => {
    trace = { ...trace, run: run({ trigger: "channel", trigger_ref: "telegram:1001" }) };
    renderPage();
    const value = await trigger();
    expect(value.getByText("channel")).not.toBeNull();
    expect(value.getByText("telegram:1001").className).toContain("font-mono");
    expect(value.queryByRole("link")).toBeNull();
    expect(within(screen.getByRole("region", { name: "Run summary" })).queryByText("Delegated")).toBeNull();
  });

  it("encodes the conversation id in the link", async () => {
    trace = { ...trace, run: run({ trigger: "channel", trigger_ref: "conversation:a/b c" }) };
    renderPage();
    const value = await trigger();
    expect(value.getByRole("link", { name: "Delegated from a conversation" }).getAttribute("href")).toBe("/channels/a%2Fb%20c");
  });

  it("shows any other trigger as it is", async () => {
    trace = { ...trace, run: run({ trigger: "schedule", trigger_ref: "s1@2026-10-15T10:00:00Z" }) };
    renderPage();
    const value = await trigger();
    expect(value.getByText("schedule")).not.toBeNull();
    expect(value.getByText("s1@2026-10-15T10:00:00Z")).not.toBeNull();
    expect(value.queryByText("Delegated")).toBeNull();
  });
});

describe("RunDetailPage project runs", () => {
  const header = async () => within(await screen.findByRole("region", { name: "Run summary" }));
  const mrCall = (over: Partial<ToolCall> = {}): ToolCall => ({
    ...toolCall,
    id: "t-mr",
    tool_name: "yuno__open_merge_request",
    input: { title: "Add src", description: "d" },
    output: { number: 12, url: "https://gitlab.test/g/p/-/merge_requests/12", branch: "yuno/0123456789ab", provider: "gitlab" },
    status: "executed",
    is_error: false,
    ...over,
  });

  it("shows the toolchain badge in the header and the merge request in a card of its own", async () => {
    trace = { ...trace, run: run({ toolchain: "go", merge_request: mergeRequest({ run_id: RUN }) }), tool_calls: [mrCall()] };
    renderPage();
    const h = await header();
    expect(h.getByText("Toolchain: Go")).not.toBeNull();
    expect(h.queryByRole("link", { name: /Merge request/ })).toBeNull();
    const card = within(await screen.findByRole("region", { name: "Merge request" }));
    const link = card.getByRole("link", { name: "Merge request !12" });
    expect(link.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(card.getByText("Open")).not.toBeNull();
    // the card sits right under the summary
    expect(screen.getByRole("region", { name: "Run summary" }).nextElementSibling).toBe(screen.getByRole("region", { name: "Merge request" }));
  });

  it("calls a GitHub request a pull request", async () => {
    trace = {
      ...trace,
      run: run({ toolchain: "node", merge_request: mergeRequest({ run_id: RUN, provider: "github", number: 3, url: "https://github.test/o/r/pull/3" }) }),
    };
    renderPage();
    const h = await header();
    expect(h.getByText("Toolchain: Node")).not.toBeNull();
    const card = within(await screen.findByRole("region", { name: "Pull request" }));
    expect(card.getByRole("link", { name: "Pull request #3" }).getAttribute("href")).toBe("https://github.test/o/r/pull/3");
  });

  it("shows neither a toolchain nor a card without a checkout, and looks nothing up while the call waits or failed", async () => {
    trace = { ...trace, tool_calls: [mrCall({ status: "awaiting_approval", output: undefined }), mrCall({ id: "t-mr2", status: "failed", is_error: true, output: { error: "boom" } })] };
    renderPage();
    const h = await header();
    await screen.findByText("Live");
    expect(h.queryByText(/Toolchain/)).toBeNull();
    expect(screen.queryByRole("region", { name: "Merge request" })).toBeNull();
    expect(calls.some((c) => c.url.startsWith("/api/merge-requests"))).toBe(false);
  });

  it("picks up the record when the tool call that opened the request arrives on the stream", async () => {
    trace = { ...trace, run: run({ toolchain: "python" }) };
    requestsForRun = [mergeRequest({ run_id: RUN, number: 7, title: "Fix the build" })];
    renderPage();
    const h = await header();
    expect(h.getByText("Toolchain: Python")).not.toBeNull();
    await screen.findByText("Live");
    expect(screen.queryByRole("region", { name: "Merge request" })).toBeNull();
    send(7, "tool_call", { tool_call: mrCall() });
    expect(await screen.findByRole("link", { name: "Merge request !7" })).not.toBeNull();
    expect(calls.some((c) => c.url === `/api/merge-requests?run_id=${RUN}&limit=1`)).toBe(true);
  });

  it("keeps the card when a later run.status event carries the run without its record", async () => {
    trace = { ...trace, run: run({ merge_request: mergeRequest({ run_id: RUN }) }) };
    renderPage();
    await screen.findByRole("region", { name: "Merge request" });
    await screen.findByText("Live");
    send(7, "run.status", { run: run({ step_count: 4 }) });
    await waitFor(() => expect(within(screen.getByRole("region", { name: "Run summary" })).getByText("4")).not.toBeNull());
    expect(screen.getByRole("region", { name: "Merge request" })).not.toBeNull();
  });

  it("replaces the record with the one a sync returns", async () => {
    trace = { ...trace, run: run({ merge_request: mergeRequest({ run_id: RUN, synced_at: new Date().toISOString() }) }) };
    synced = mergeRequest({ run_id: RUN, state: "merged", closed_at: "2026-10-15T11:00:00Z" });
    renderPage();
    const card = within(await screen.findByRole("region", { name: "Merge request" }));
    fireEvent.click(card.getByRole("button", { name: "Sync" }));
    await waitFor(() => expect(card.getByText("Merged")).not.toBeNull());
  });

  it("names the project the run works on, linking to its page", async () => {
    trace = {
      ...trace,
      run: run({
        project_id: "p1",
        project: { name: "yuno-playground", repo_url: "https://gitlab.com/g/p.git", provider: "gitlab", default_branch: "main", sandbox_network: false },
      }),
    };
    renderPage();
    const h = await header();
    const item = h.getByText("Project").nextElementSibling as HTMLElement;
    expect(within(item).getByRole("link", { name: "yuno-playground" }).getAttribute("href")).toBe("/projects/p1");
  });

  it("has no Project item for a run without a project", async () => {
    renderPage();
    const h = await header();
    expect(h.getByText("Trigger")).not.toBeNull();
    expect(h.queryByText("Project")).toBeNull();
  });
});
