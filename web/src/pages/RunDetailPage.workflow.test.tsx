// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { AgentMessage, Approval, Run, RunBranch, RunNode, ToolCall, Trace } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import RunDetailPage from "./RunDetailPage";

// The run page of a multi-node workflow run: approvals with their buttons, the messages the
// nodes passed on, the run state, and nodes that ran more than once.

const RUN = "r1";
const EVENTS = `/api/runs/${RUN}/events`;

const run = (over: Partial<Run> = {}): Run => ({
  id: RUN,
  workflow_id: "wf",
  workflow_version: 1,
  trigger: "manual",
  status: "running",
  input: { task: "fizzbuzz" },
  state: {},
  limits: {},
  step_count: 4,
  tool_call_count: 2,
  cost_usd_micros: 40_000,
  created_at: "2026-10-15T10:00:00Z",
  started_at: "2026-10-15T10:00:01Z",
  ...over,
});

const node = (id: string, node_id: string, visit: number, over: Partial<RunNode> = {}): RunNode => ({
  id,
  run_id: RUN,
  node_id,
  visit,
  iteration: 1,
  status: "succeeded",
  started_at: `2026-10-15T10:0${visit}:0${id.length}Z`,
  ...over,
});

const approval = (over: Partial<Approval> = {}): Approval => ({
  id: "ap1",
  run_id: RUN,
  run_node_id: "rn-gate",
  kind: "node",
  node_id: "gate",
  message: "Deploy release 1.0.0?",
  status: "pending",
  requested_at: "2026-10-15T10:05:00Z",
  expires_at: "2026-10-16T10:05:00Z",
  ...over,
});

const message = (over: Partial<AgentMessage> = {}): AgentMessage => ({
  id: "m1",
  run_id: RUN,
  from_node: "coder",
  to_node: "reviewer",
  payload: { summary: "wrote fizzbuzz.py" },
  created_at: "2026-10-15T10:02:00Z",
  ...over,
});

let trace: Trace;
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  trace = { run: run(), nodes: [], model_calls: [], tool_calls: [], spans: [], agent_messages: [], approvals: [] };
  api = installFakeApi();
  api.on("GET", `/api/runs/${RUN}/trace`, () => trace);
  api.on("GET", `/api/runs/${RUN}/artifacts`, () => []);
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

const section = async (name: string) => within((await screen.findByRole("heading", { name: new RegExp(`^${name}`) })).closest("section")!);

describe("approvals on the run page", () => {
  beforeEach(() => {
    trace.run = run({ status: "waiting_approval", current_node: "gate" });
    trace.nodes = [node("rn-gate", "gate", 1, { status: "waiting_approval" })];
    trace.approvals = [approval()];
  });

  it("shows what the run waits for, with the buttons to decide it", async () => {
    renderPage();
    const approvals = await section("Approvals");
    expect(approvals.getByRole("status").textContent).toBe("This run is paused until this request is decided (node gate).");
    const card = within(approvals.getByRole("article"));
    expect(card.getByText("Deploy release 1.0.0?")).not.toBeNull();
    expect(card.getByText("Workflow step")).not.toBeNull();
    expect(card.getByRole("button", { name: "Approve" })).not.toBeNull();
    expect(card.getByRole("button", { name: "Reject" })).not.toBeNull();
    expect(card.queryByRole("link")).toBeNull();

    // the node card says it is waiting and the header names the node
    const header = screen.getByRole("region", { name: "Run summary" });
    expect(within(header).getByText("gate")).not.toBeNull();
    expect(within(header).getByText("Current node")).not.toBeNull();
  });

  it("approves with the reason and shows the decision at once", async () => {
    api.on("POST", "/api/approvals/ap1/approve", () => approval({ status: "approved", decided_at: "2026-10-15T10:06:00Z", decided_by: "api", reason: "go" }));
    renderPage();
    const approvals = await section("Approvals");
    fireEvent.change(approvals.getByLabelText("Reason"), { target: { value: "go" } });
    fireEvent.click(approvals.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approvals.queryByRole("button", { name: "Approve" })).toBeNull());
    expect(api.made("POST", "/api/approvals/ap1/approve")[0].body).toEqual({ reason: "go" });
    const card = within(approvals.getByRole("article"));
    expect(card.getByText("approved")).not.toBeNull();
    expect(card.getByText(new RegExp(`Decided ${formatDateTime("2026-10-15T10:06:00Z")} by api · go`))).not.toBeNull();
  });

  it("rejects with a reason", async () => {
    api.on("POST", "/api/approvals/ap1/reject", () => approval({ status: "rejected", decided_by: "api", decided_at: "2026-10-15T10:06:00Z" }));
    renderPage();
    const approvals = await section("Approvals");
    fireEvent.change(approvals.getByLabelText("Reason"), { target: { value: "not yet" } });
    fireEvent.click(approvals.getByRole("button", { name: "Reject" }));
    await waitFor(() => expect(approvals.getByText("rejected")).not.toBeNull());
    expect(api.made("POST", "/api/approvals/ap1/reject")[0].body).toEqual({ reason: "not yet" });
  });

  it("says so when the approval was already decided", async () => {
    api.on("POST", "/api/approvals/ap1/approve", () => apiError(409, "conflict", "approval already decided"));
    renderPage();
    const approvals = await section("Approvals");
    fireEvent.click(approvals.getByRole("button", { name: "Approve" }));
    expect(await approvals.findByText("This approval was already decided.")).not.toBeNull();
  });

  it("follows the stream: a new request appears, a decision updates it, the pause note goes when the run resumes", async () => {
    renderPage();
    const approvals = await section("Approvals");
    await screen.findByText("Live");

    api.send(EVENTS, "approval.requested", { approval: approval({ id: "ap2", node_id: "deployer", kind: "tool", message: "Deploy with deploy_release", requested_at: "2026-10-15T10:06:00Z" }) });
    expect(await approvals.findByText("Deploy with deploy_release")).not.toBeNull();
    expect(approvals.getByRole("status").textContent).toBe("This run is paused until these 2 requests are decided.");

    api.send(EVENTS, "approval.decided", { approval: approval({ status: "approved", decided_at: "2026-10-15T10:07:00Z", decided_by: "telegram" }) });
    await waitFor(() => expect(within(approvals.getAllByRole("article")[1]).getByText("approved")).not.toBeNull());
    expect(approvals.getByRole("status").textContent).toBe("This run is paused until this request is decided (node deployer).");

    api.send(EVENTS, "run.status", { run: run({ status: "running", current_node: "deployer" }) });
    await waitFor(() => expect(approvals.queryByRole("status")).toBeNull());
  });

  it("shows the tool a tool approval is about, with its input", async () => {
    trace.tool_calls = [
      {
        id: "tc1",
        run_id: RUN,
        run_node_id: "rn-gate",
        tool_use_id: "mcp-1",
        tool_name: "deploy_release",
        input: { version: "1.2.0" },
        is_error: false,
        status: "awaiting_approval",
        attempt: 1,
        duration_ms: 0,
      } satisfies ToolCall,
    ];
    trace.approvals = [approval({ kind: "tool", tool_call_id: "tc1", message: "Deploy 1.2.0" })];
    renderPage();
    const approvals = await section("Approvals");
    const card = within(approvals.getByRole("article"));
    expect(card.getByText("deploy_release")).not.toBeNull();
    expect(card.getByText(/Input to deploy_release/).closest("details")!.querySelector("pre")!.textContent).toContain('"version": "1.2.0"');
  });

  it.each([
    ["succeeded", true],
    ["failed", true],
    ["cancelled", true],
    ["timed_out", true],
    ["running", false],
    ["waiting_approval", false],
  ] as const)("a pending tool approval on a run that is %s shows the ended-run note: %s", async (status, shown) => {
    trace.run = run({ status });
    trace.approvals = [approval({ kind: "tool", message: "Open the merge request" })];
    renderPage();
    const card = within((await section("Approvals")).getByRole("article"));
    if (shown) expect(card.getByRole("note").textContent).toBe("This run has already ended. Approving still executes the call; rejecting discards it.");
    else expect(card.queryByRole("note")).toBeNull();
  });

  it("shows no ended-run note on a workflow step of an ended run, nor once the late request is decided", async () => {
    trace.run = run({ status: "failed" });
    trace.approvals = [
      approval({ id: "ap-node" }),
      approval({ id: "ap-done", kind: "tool", status: "approved", decided_at: "2026-10-15T10:06:00Z", decided_by: "api" }),
    ];
    renderPage();
    await section("Approvals");
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("offers Approve always for a command approval only", async () => {
    trace.approvals = [approval({ kind: "exec", message: "pip install requests" })];
    renderPage();
    expect((await section("Approvals")).getByRole("button", { name: "Approve always" })).not.toBeNull();
  });

  it("notes a paused run even before its approval has been read", async () => {
    trace.approvals = [];
    renderPage();
    const approvals = await section("Approvals");
    expect(approvals.getByRole("status").textContent).toBe("This run is paused, waiting for an approval.");
  });

  it("has no approvals section for a run that never asked", async () => {
    trace.run = run({ status: "succeeded", finished_at: "2026-10-15T10:09:00Z" });
    trace.approvals = [];
    renderPage();
    await screen.findByRole("heading", { name: "Timeline" });
    expect(screen.queryByRole("heading", { name: /^Approvals/ })).toBeNull();
  });
});

describe("agent messages", () => {
  it("lists who sent what to whom, oldest first, and follows the stream", async () => {
    trace.agent_messages = [
      message({ id: "m2", from_node: "reviewer", to_node: "coder", payload: { verdict: "changes_requested" }, created_at: "2026-10-15T10:04:00Z" }),
      message(),
    ];
    renderPage();
    const messages = await section("Agent messages");
    const items = messages.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText("coder")).not.toBeNull();
    expect(within(items[0]).getByText("reviewer")).not.toBeNull();
    expect(items[0].textContent).toContain("wrote fizzbuzz.py");
    expect(items[0].textContent).toContain(formatDateTime("2026-10-15T10:02:00Z"));
    expect(items[1].textContent).toContain("changes_requested");

    await screen.findByText("Live");
    api.send(EVENTS, "agent_message", { agent_message: message({ id: "m3", from_node: "coder", to_node: "reviewer", payload: { summary: "fixed" }, created_at: "2026-10-15T10:06:00Z" }) });
    await waitFor(() => expect(messages.getAllByRole("listitem")).toHaveLength(3));
    expect(messages.getAllByRole("listitem")[2].textContent).toContain('"summary": "fixed"');
  });

  it("is absent until a node has passed something on", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Timeline" });
    expect(screen.queryByRole("heading", { name: /^Agent messages/ })).toBeNull();
    await screen.findByText("Live");
    api.send(EVENTS, "agent_message", { agent_message: message() });
    expect(await screen.findByRole("heading", { name: /^Agent messages/ })).not.toBeNull();
  });
});

describe("run state", () => {
  it("shows the state and replaces it when the run status event carries a new one", async () => {
    trace.run = run({ state: { coder: { summary: "v1", files: ["a.py"] } } });
    renderPage();
    const state = await section("State");
    expect(state.getByText("Run state").closest("details")!.querySelector("pre")!.textContent).toContain('"summary": "v1"');

    await screen.findByText("Live");
    api.send(EVENTS, "run.status", { run: run({ current_node: "reviewer", state: { coder: { summary: "v2", files: ["a.py"] }, reviewer: { verdict: "approved" } } }) });
    await waitFor(() => expect(state.getByText("Run state").closest("details")!.querySelector("pre")!.textContent).toContain('"verdict": "approved"'));
    expect(state.getByText("Run state").closest("details")!.querySelector("pre")!.textContent).toContain('"summary": "v2"');
    expect(within(screen.getByRole("region", { name: "Run summary" })).getByText("reviewer")).not.toBeNull();
  });

  it("says the state is empty before a node has finished", async () => {
    renderPage();
    expect((await section("State")).getByText("Empty until a node finishes.")).not.toBeNull();
  });
});

describe("several nodes and repeat visits", () => {
  beforeEach(() => {
    trace.run = run({ status: "running", current_node: "reviewer" });
    trace.nodes = [
      node("rn1", "start", 1, { iteration: 0, started_at: "2026-10-15T10:00:01Z" }),
      node("rn2", "coder", 1, { started_at: "2026-10-15T10:00:02Z" }),
      node("rn3", "reviewer", 1, { started_at: "2026-10-15T10:01:00Z" }),
      node("rn4", "coder", 2, { started_at: "2026-10-15T10:02:00Z" }),
      node("rn5", "reviewer", 2, { status: "running", started_at: "2026-10-15T10:03:00Z" }),
    ];
  });

  it("lists every visit in the order it started and numbers the visits of a node that ran again", async () => {
    renderPage();
    const timeline = await section("Timeline");
    const cards = timeline.getAllByRole("region");
    expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual([
      "Node start",
      "Node coder, visit 1",
      "Node reviewer, visit 1",
      "Node coder, visit 2",
      "Node reviewer, visit 2",
    ]);
    expect(within(cards[3]).getByText("visit 2")).not.toBeNull();
    expect(within(cards[1]).getByText("visit 1")).not.toBeNull();
    expect(within(cards[0]).queryByText(/^visit/)).toBeNull();
    expect(within(cards[4]).getByText("running")).not.toBeNull();
  });

  it("adds a visit as it starts", async () => {
    renderPage();
    const timeline = await section("Timeline");
    await screen.findByText("Live");
    api.send(EVENTS, "node.status", { node: node("rn6", "gate", 1, { status: "waiting_approval", started_at: "2026-10-15T10:04:00Z" }) });
    await waitFor(() => expect(timeline.getAllByRole("region")).toHaveLength(6));
    const gate = within(timeline.getByRole("region", { name: "Node gate" }));
    expect(gate.getByText("waiting approval")).not.toBeNull();
    expect(gate.getByText("Waiting for an approval.")).not.toBeNull();
  });
});

describe("a run that asks the operator a question", () => {
  const question = (over: Partial<Approval> = {}): Approval =>
    approval({
      id: "q1",
      run_node_id: "rn-coder",
      kind: "question",
      node_id: "coder",
      message: "Which greeting language should I use?",
      options: ["English", "Spanish"],
      requested_at: "2026-10-15T10:05:00Z",
      ...over,
    });

  beforeEach(() => {
    trace.run = run({ status: "waiting_approval", current_node: "coder" });
    trace.nodes = [node("rn-coder", "coder", 1, { status: "waiting_approval" })];
    trace.approvals = [question()];
  });

  it("puts the question to the operator on the Approvals section, with the header line pointing to it", async () => {
    renderPage();
    const approvals = await section("Approvals");
    const card = within(approvals.getByRole("article"));
    expect(card.getByText("Question")).not.toBeNull();
    expect(card.getByText("Which greeting language should I use?")).not.toBeNull();
    expect(card.getByRole("button", { name: "English" })).not.toBeNull();
    expect(card.getByRole("button", { name: "Spanish" })).not.toBeNull();
    expect(card.getByLabelText("Your answer")).not.toBeNull();
    expect(card.getByRole("button", { name: "Decline" })).not.toBeNull();
    expect(card.queryByRole("button", { name: "Approve" })).toBeNull();

    const link = within(screen.getByRole("region", { name: "Run summary" })).getByRole("link", { name: /^Asks:/ });
    expect(link.textContent).toBe("Asks: Which greeting language should I use?");
    expect(link.getAttribute("href")).toBe("#approvals");
    // the fragment names the section the question is in
    const target = document.getElementById("approvals")!;
    expect(target.tagName).toBe("SECTION");
    expect(target.contains(card.getByText("Which greeting language should I use?"))).toBe(true);
  });

  it("answers with an option: the card shows the answer, and the header line goes", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => question({ status: "approved", answer: "Spanish", decided_at: "2026-10-15T10:06:00Z", decided_by: "api" }));
    renderPage();
    const approvals = await section("Approvals");
    fireEvent.click(approvals.getByRole("button", { name: "Spanish" }));
    expect(await approvals.findByText("Answered: Spanish")).not.toBeNull();
    expect(api.made("POST", "/api/approvals/q1/answer")[0].body).toEqual({ answer: "Spanish" });
    expect(approvals.queryByRole("button", { name: "Spanish" })).toBeNull();
    expect(screen.queryByRole("link", { name: /^Asks:/ })).toBeNull();
  });

  it("follows the stream: an answer from Telegram shows on the card, and the header line goes when the run resumes", async () => {
    renderPage();
    const approvals = await section("Approvals");
    await screen.findByText("Live");
    api.send(EVENTS, "approval.decided", { approval: question({ status: "approved", answer: "English", decided_at: "2026-10-15T10:06:00Z", decided_by: "chat:Chief of Staff" }) });
    expect(await approvals.findByText("Answered: English")).not.toBeNull();
    expect(approvals.getByText(/by chat:Chief of Staff/)).not.toBeNull();
    api.send(EVENTS, "run.status", { run: run({ status: "running", current_node: "coder" }) });
    await waitFor(() => expect(screen.queryByRole("link", { name: /^Asks:/ })).toBeNull());
  });

  it("shows a question asked while the page is open", async () => {
    trace.run = run({ status: "running", current_node: "coder" });
    trace.approvals = [];
    renderPage();
    await screen.findByRole("heading", { name: "Timeline" });
    await screen.findByText("Live");
    expect(screen.queryByRole("link", { name: /^Asks:/ })).toBeNull();
    api.send(EVENTS, "run.status", { run: run({ status: "waiting_approval", current_node: "coder" }) });
    api.send(EVENTS, "approval.requested", { approval: question() });
    expect(await screen.findByRole("link", { name: "Asks: Which greeting language should I use?" })).not.toBeNull();
    expect((await section("Approvals")).getByRole("button", { name: "English" })).not.toBeNull();
  });

  it("shows the question and its answer in the timeline, and says why the step has a second visit", async () => {
    trace.run = run({ status: "running", current_node: "coder" });
    trace.nodes = [
      node("rn-coder", "coder", 1, { started_at: "2026-10-15T10:01:00Z" }),
      node("rn-coder2", "coder", 2, { status: "running", started_at: "2026-10-15T10:07:00Z", question_id: "q1" }),
    ];
    trace.approvals = [question({ status: "approved", answer: "Spanish", decided_at: "2026-10-15T10:06:00Z", decided_by: "api" })];
    renderPage();
    const timeline = await section("Timeline");
    const first = within(timeline.getByRole("region", { name: "Node coder, visit 1" }));
    expect(first.getByText("Question")).not.toBeNull();
    expect(first.getByText("Answered: Spanish")).not.toBeNull();
    const second = within(timeline.getByRole("region", { name: "Node coder, visit 2" }));
    expect(second.getByText("visit 2, continued after a question")).not.toBeNull();
  });
});

describe("a run with a fork", () => {
  const at = (sec: number) => `2026-10-15T10:00:${String(sec).padStart(2, "0")}Z`;
  const branch = (head: string, over: Partial<RunBranch> = {}): RunBranch => ({
    id: `b-${head}`,
    run_id: RUN,
    fork_visit_id: "rn-panel",
    fork_id: "panel",
    head,
    status: "running",
    current_node: head,
    started_at: at(10),
    ...over,
  });
  const visit = (head: string, over: Partial<RunNode> = {}): RunNode => ({
    id: `rn-${head}`,
    run_id: RUN,
    node_id: head,
    agent_id: `agent-${head}`,
    branch_id: `b-${head}`,
    visit: 1,
    iteration: 1,
    status: "running",
    started_at: at(11),
    ...over,
  });
  const heads = ["security", "performance", "maintainability"];

  beforeEach(() => {
    trace.run = run({ status: "running", current_node: "panel" });
    trace.nodes = [
      node("rn-coder", "coder", 1, { started_at: at(1) }),
      { id: "rn-panel", run_id: RUN, node_id: "panel", visit: 1, iteration: 0, status: "running", started_at: at(10), output: { branches: heads } },
      ...heads.map((h) => visit(h)),
    ];
    trace.branches = heads.map((h) => branch(h));
  });

  const header = () => within(screen.getByRole("region", { name: "Run summary" }));

  it("shows the fork as one card with a column for each branch, and the header counts the branches running", async () => {
    renderPage();
    const timeline = await section("Timeline");
    const fork = within(timeline.getByRole("region", { name: "Fork panel" }));
    expect(fork.getAllByRole("region", { name: /^Branch / }).map((c) => c.getAttribute("aria-label"))).toEqual(["Branch security", "Branch performance", "Branch maintainability"]);
    expect(timeline.getByRole("region", { name: "Node security, branch security" })).not.toBeNull();
    expect(timeline.getByRole("region", { name: "Node coder" })).not.toBeNull();
    expect(header().getByText("panel (3 branches running)")).not.toBeNull();
  });

  it("follows the stream: a branch ends, another waits for an answer, the join passes and the run moves on", async () => {
    renderPage();
    const timeline = await section("Timeline");
    await screen.findByText("Live");

    api.send(EVENTS, "branch.status", { branch: branch("security", { status: "succeeded", finished_at: at(30) }) });
    await waitFor(() => expect(header().getByText("panel (2 branches running)")).not.toBeNull());
    const head = (name: string) => within(timeline.getByRole("region", { name: `Branch ${name}` }).querySelector("header") as HTMLElement);
    expect(head("security").getByText("succeeded")).not.toBeNull();

    api.send(EVENTS, "branch.status", { branch: branch("performance", { status: "waiting_approval" }) });
    await waitFor(() => expect(head("performance").getByText("Waiting for an answer")).not.toBeNull());
    expect(header().getByText("panel (1 branch running, 1 waiting for an answer)")).not.toBeNull();

    api.send(EVENTS, "branch.status", { branch: branch("maintainability", { status: "failed", failure_reason: "no verdict", finished_at: at(35) }) });
    await waitFor(() => expect(within(timeline.getByRole("region", { name: "Branch maintainability" })).getByText("no verdict")).not.toBeNull());
    expect(header().getByText("panel (1 waiting for an answer)")).not.toBeNull();

    // a branch of another run is not shown
    api.send(EVENTS, "branch.status", { branch: { ...branch("elsewhere"), run_id: "other" } });
    for (const h of heads) {
      api.send(EVENTS, "branch.status", { branch: branch(h, { status: "succeeded", finished_at: at(40) }) });
    }
    api.send(EVENTS, "node.status", { node: { id: "rn-merge", run_id: RUN, node_id: "merge", visit: 1, iteration: 0, status: "succeeded", started_at: at(41), finished_at: at(41), output: { branches: heads } } });
    expect(await timeline.findByText("Joined 3 branches")).not.toBeNull();
    expect(timeline.queryByRole("region", { name: "Branch elsewhere" })).toBeNull();

    api.send(EVENTS, "run.status", { run: run({ status: "running", current_node: "lead" }) });
    await waitFor(() => expect(header().getByText("lead")).not.toBeNull());
    expect(header().queryByText(/branches? running/)).toBeNull();
  });

  it("shows the branch's reason when a run fails inside a branch, and no step line", async () => {
    trace.run = run({ status: "failed", current_node: "panel", failure_reason: "branch security failed: tool exec failed" });
    trace.branches = [
      branch("security", { status: "failed", failure_reason: "tool exec failed", finished_at: at(20) }),
      branch("performance", { status: "cancelled", failure_reason: "stopped: branch security failed", finished_at: at(21) }),
      branch("maintainability", { status: "cancelled", failure_reason: "stopped: branch security failed", finished_at: at(21) }),
    ];
    renderPage();
    const timeline = await section("Timeline");
    expect(within(timeline.getByRole("region", { name: "Branch security" })).getByText("tool exec failed")).not.toBeNull();
    expect(within(timeline.getByRole("region", { name: "Branch performance" })).getByText("stopped: branch security failed")).not.toBeNull();
    expect(header().queryByText("Current node")).toBeNull();
  });
});
