// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { Approval, ModelCall, RunBranch, RunNode } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { toRunView, type RetryMark } from "@/lib/trace";
import { run } from "@/test/fixtures";
import Timeline from "./Timeline";

afterEach(cleanup);

const node = (over: Partial<RunNode>): RunNode => ({ id: "n1", run_id: "r1", node_id: "coder", visit: 1, iteration: 1, status: "failed", started_at: "2026-10-15T10:00:01Z", ...over });

function renderTimeline(retries: RetryMark[], nodes: RunNode[] = [node({}), node({ id: "n2", visit: 2, status: "succeeded", started_at: "2026-10-15T10:07:00Z" })]) {
  const view = toRunView({ run: run(), nodes, model_calls: [], tool_calls: [], spans: [], agent_messages: [], approvals: [] }, [], retries);
  render(<Timeline view={view} active={false} now={Date.parse("2026-10-15T10:10:00Z")} />);
}

const before = (a: Element, b: Element) => (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;

describe("the retry marker", () => {
  it("sits between the visit that failed and the visit that followed the retry", () => {
    renderTimeline([{ retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" }]);
    const first = screen.getByRole("region", { name: "Node coder, visit 1" });
    const second = screen.getByRole("region", { name: "Node coder, visit 2" });
    const marker = screen.getByText("Retry 1 from coder");
    expect(before(first, marker)).toBe(true);
    expect(before(marker, second)).toBe(true);
  });

  it("shows the time of the retry and who asked for it", () => {
    renderTimeline([{ retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" }]);
    const row = screen.getByText("Retry 1 from coder").parentElement as HTMLElement;
    expect(row.textContent).toContain(formatDateTime("2026-10-15T10:06:00Z"));
    expect(row.textContent).toContain("by operator");
  });

  it("is a list item of the timeline, one for each retry", () => {
    renderTimeline(
      [
        { retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" },
        { retries: 2, from_node: "reviewer", at: "2026-10-15T10:08:00Z" },
      ],
      [node({}), node({ id: "n2", visit: 2, started_at: "2026-10-15T10:07:00Z" }), node({ id: "n3", node_id: "reviewer", visit: 1, started_at: "2026-10-15T10:09:00Z" })],
    );
    const items = screen.getAllByRole("listitem").filter((li) => li.parentElement?.tagName === "OL");
    expect(items).toHaveLength(5);
    expect(screen.getByText("Retry 1 from coder").closest("li")).toBe(items[1]);
    expect(screen.getByText("Retry 2 from reviewer").closest("li")).toBe(items[3]);
    expect(screen.queryByText(/by /)).toBeNull();
  });

  it("is absent when the run was never retried", () => {
    renderTimeline([]);
    expect(screen.queryByText(/^Retry \d/)).toBeNull();
    expect(screen.getAllByRole("region")).toHaveLength(2);
  });
});

describe("a continuation visit", () => {
  const visits = [node({ status: "succeeded" }), node({ id: "n2", visit: 2, status: "succeeded", started_at: "2026-10-15T10:07:00Z", question_id: "q1" })];

  function renderVisits(nodes: RunNode[]) {
    const view = toRunView({ run: run(), nodes, model_calls: [], tool_calls: [], spans: [], agent_messages: [], approvals: [] });
    render(<Timeline view={view} active={false} now={Date.parse("2026-10-15T10:10:00Z")} />);
  }

  it("says why the step ran twice", () => {
    renderVisits(visits);
    const second = within(screen.getByRole("region", { name: "Node coder, visit 2" }));
    expect(second.getByText("visit 2, continued after a question")).not.toBeNull();
    const first = within(screen.getByRole("region", { name: "Node coder, visit 1" }));
    expect(first.getByText("visit 1")).not.toBeNull();
    expect(first.queryByText(/continued after a question/)).toBeNull();
  });

  it("numbers a second visit that follows a loop or a retry as before", () => {
    renderVisits([node({ status: "succeeded" }), node({ id: "n2", visit: 2, status: "succeeded", started_at: "2026-10-15T10:07:00Z" })]);
    expect(screen.getByText("visit 2")).not.toBeNull();
    expect(screen.queryByText(/continued after a question/)).toBeNull();
  });
});

describe("the approval rows of a question", () => {
  const question = (over: Partial<Approval> = {}): Approval => ({
    id: "q1",
    run_id: "r1",
    run_node_id: "n1",
    kind: "question",
    node_id: "coder",
    message: "Which greeting language should I use?",
    options: ["English", "Spanish"],
    status: "pending",
    requested_at: "2026-10-15T10:02:00Z",
    expires_at: "2026-10-16T10:02:00Z",
    ...over,
  });

  function renderApprovals(approvals: Approval[]) {
    const view = toRunView({ run: run(), nodes: [node({ status: "waiting_approval" })], model_calls: [], tool_calls: [], spans: [], agent_messages: [], approvals });
    render(<Timeline view={view} active now={Date.parse("2026-10-15T10:10:00Z")} />);
    return within(screen.getByRole("region", { name: "Node coder" }));
  }

  it("calls the kind Question and shows the question", () => {
    const card = renderApprovals([question()]);
    expect(card.getByText("Question")).not.toBeNull();
    expect(card.queryByText("question")).toBeNull();
    expect(card.getByText("Which greeting language should I use?")).not.toBeNull();
    expect(card.getByText("pending")).not.toBeNull();
    expect(card.queryByText(/Answered|Declined|Not answered/)).toBeNull();
  });

  it("shows the answer under the question once it is answered", () => {
    const card = renderApprovals([question({ status: "approved", answer: "Spanish", decided_at: "2026-10-15T10:03:00Z", decided_by: "chat:Chief of Staff" })]);
    const message = card.getByText("Which greeting language should I use?");
    const answer = card.getByText("Answered: Spanish");
    expect(before(message, answer)).toBe(true);
    expect(card.getByText("By chat:Chief of Staff")).not.toBeNull();
  });

  it("shows a decline with its reason once, and an expiry", () => {
    const declined = renderApprovals([question({ status: "rejected", reason: "not now", decided_at: "2026-10-15T10:03:00Z", decided_by: "api" })]);
    expect(declined.getByText("Declined: not now")).not.toBeNull();
    expect(declined.getAllByText(/not now/)).toHaveLength(1);
    expect(declined.getByText("By api")).not.toBeNull();
    cleanup();
    const expired = renderApprovals([question({ status: "expired" })]);
    expect(expired.getByText("Not answered in time")).not.toBeNull();
  });

  it("keeps the other kinds as they were", () => {
    const card = renderApprovals([question({ kind: "node", options: undefined, message: "Deploy?", status: "approved", decided_at: "2026-10-15T10:03:00Z", decided_by: "api", reason: "go" })]);
    expect(card.getByText("node")).not.toBeNull();
    expect(card.queryByText("Question")).toBeNull();
    expect(card.queryByText(/Answered|Declined/)).toBeNull();
    expect(card.getByText("By api · go")).not.toBeNull();
  });
});

describe("a fork visit", () => {
  const at = (sec: number) => `2026-10-15T10:00:${String(sec).padStart(2, "0")}Z`;
  const now = Date.parse("2026-10-15T10:10:00Z");

  const fork = (over: Partial<RunNode> = {}): RunNode => ({
    id: "f1",
    run_id: "r1",
    node_id: "panel",
    visit: 1,
    iteration: 0,
    status: "succeeded",
    started_at: at(10),
    finished_at: at(50),
    output: { branches: ["security", "performance"] },
    ...over,
  });
  const visit = (id: string, nodeId: string, branchId: string, over: Partial<RunNode> = {}): RunNode => ({
    id,
    run_id: "r1",
    node_id: nodeId,
    agent_id: `agent-${nodeId}`,
    branch_id: branchId,
    visit: 1,
    iteration: 2,
    status: "succeeded",
    started_at: at(11),
    finished_at: at(40),
    ...over,
  });
  const join = (over: Partial<RunNode> = {}): RunNode => ({
    id: "j1",
    run_id: "r1",
    node_id: "merge",
    visit: 1,
    iteration: 0,
    status: "succeeded",
    started_at: at(50),
    finished_at: at(50),
    output: { branches: ["security", "performance"] },
    ...over,
  });
  const branch = (over: Partial<RunBranch> = {}): RunBranch => ({
    id: "b-sec",
    run_id: "r1",
    fork_visit_id: "f1",
    fork_id: "panel",
    head: "security",
    status: "succeeded",
    current_node: "merge",
    started_at: at(10),
    finished_at: at(40),
    ...over,
  });
  const call = (over: Partial<ModelCall> = {}): ModelCall => ({
    id: "m1",
    run_id: "r1",
    run_node_id: "n-sec",
    provider: "anthropic",
    model: "claude-sonnet-5-5",
    input_tokens: 10,
    output_tokens: 5,
    cost_usd_micros: 100,
    duration_ms: 400,
    attempt: 1,
    started_at: at(12),
    ...over,
  });

  const two = [branch(), branch({ id: "b-perf", head: "performance", status: "succeeded" })];

  function renderRun(nodes: RunNode[], branches: RunBranch[], over: { model_calls?: ModelCall[]; active?: boolean } = {}) {
    const view = toRunView({ run: run({ status: "running" }), nodes, model_calls: over.model_calls ?? [], tool_calls: [], spans: [], agent_messages: [], approvals: [], branches });
    render(<Timeline view={view} active={over.active ?? true} now={now} />);
  }

  const column = (head: string) => within(screen.getByRole("region", { name: `Branch ${head}` }));
  /** The head line of a column: the branch's name and status, without the node cards below. */
  const columnHead = (head: string) => within(column(head).getByRole("heading", { name: head, level: 4 }).closest("header") as HTMLElement);

  it("is one card with the fork's id, status, times and a column for each branch", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), visit("n-perf", "performance", "b-perf")], two);
    const card = within(screen.getByRole("region", { name: "Fork panel" }));
    const head = within(card.getByRole("heading", { name: "panel", level: 3 }).closest("header") as HTMLElement);
    expect(head.getByText("succeeded")).not.toBeNull();
    expect(head.getByText("Fork")).not.toBeNull();
    expect(head.getByText(`Started ${formatDateTime(at(10))} · Finished ${formatDateTime(at(50))} · 40s`)).not.toBeNull();
    const columns = card.getAllByRole("region", { name: /^Branch / });
    expect(columns.map((c) => c.getAttribute("aria-label"))).toEqual(["Branch security", "Branch performance"]);
    expect(within(columns[0]).getByRole("heading", { name: "security", level: 4 }).className).toContain("font-mono");
    expect(within(columns[1]).getByRole("heading", { name: "performance", level: 4 })).not.toBeNull();
  });

  it("puts the node cards of each branch in its own column, named with their branch", () => {
    renderRun(
      [fork(), visit("n-sec", "security", "b-sec"), visit("n-sec2", "security_deep", "b-sec", { started_at: at(20) }), visit("n-perf", "performance", "b-perf")],
      two,
    );
    const security = column("security");
    const cards = security.getAllByRole("region", { name: /^Node / });
    expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual(["Node security, branch security", "Node security_deep, branch security"]);
    expect(column("performance").getAllByRole("region", { name: /^Node / }).map((c) => c.getAttribute("aria-label"))).toEqual(["Node performance, branch performance"]);
    // the node cards are the ordinary ones: node id, status, turns
    expect(within(cards[0]).getByRole("heading", { name: "security" })).not.toBeNull();
    expect(within(cards[0]).getByText("2 turns")).not.toBeNull();
    // and nothing of a branch is listed outside the fork card
    expect(screen.getAllByRole("region", { name: /^Node / })).toHaveLength(3);
  });

  it("keeps the heading levels in order: the fork card, then the branches, then the node cards inside them", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), visit("n-perf", "performance", "b-perf"), join(), visit("n-lead", "lead", "")], two);
    expect(screen.getByRole("heading", { name: "panel" }).tagName).toBe("H3");
    expect(column("security").getByRole("heading", { name: "security", level: 4 })).not.toBeNull();
    const card = within(screen.getByRole("region", { name: "Node security, branch security" }));
    expect(card.getByRole("heading", { name: "security", level: 5 })).not.toBeNull();
    expect(column("security").queryByRole("heading", { level: 3 })).toBeNull();
    // the join and a node outside any fork keep the top level
    expect(screen.getByRole("heading", { name: "merge" }).tagName).toBe("H3");
    expect(within(screen.getByRole("region", { name: "Node lead" })).getByRole("heading", { name: "lead", level: 3 })).not.toBeNull();
  });

  it("shows a branch status badge in each column, and reads a parked branch as waiting for an answer", () => {
    renderRun(
      [fork({ status: "running", finished_at: undefined }), visit("n-sec", "security", "b-sec", { status: "waiting_approval", finished_at: undefined }), visit("n-perf", "performance", "b-perf", { status: "running", finished_at: undefined })],
      [branch({ status: "waiting_approval", finished_at: undefined }), branch({ id: "b-perf", head: "performance", status: "running", finished_at: undefined })],
    );
    expect(columnHead("security").getByText("Waiting for an answer")).not.toBeNull();
    expect(columnHead("security").queryByText("waiting approval")).toBeNull();
    expect(columnHead("performance").getByText("running")).not.toBeNull();
    // the node card inside a parked branch keeps the node's own words
    expect(within(screen.getByRole("region", { name: "Node security, branch security" })).getByText("waiting approval")).not.toBeNull();
  });

  it("shows a failed branch's reason in its column, the other column untouched", () => {
    renderRun(
      [fork({ status: "failed" }), visit("n-sec", "security", "b-sec", { status: "failed", failure_reason: "tool exec failed" }), visit("n-perf", "performance", "b-perf", { status: "failed", failure_reason: "stopped: branch security failed" })],
      [branch({ status: "failed", failure_reason: "reviewer gave no verdict" }), branch({ id: "b-perf", head: "performance", status: "cancelled", failure_reason: "stopped: branch security failed" })],
    );
    const alerts = column("security").getAllByRole("alert");
    expect(alerts.map((a) => a.textContent)).toEqual(["reviewer gave no verdict", "tool exec failed"]);
    expect(columnHead("security").getByText("failed")).not.toBeNull();
    expect(columnHead("performance").getByText("cancelled")).not.toBeNull();
    expect(column("performance").queryByText("reviewer gave no verdict")).toBeNull();
  });

  it("shows the reason of a branch the fork stopped as a plain note, and keeps the alert for the branch that failed", () => {
    renderRun(
      [fork({ status: "failed" })],
      [branch({ status: "failed", failure_reason: "reviewer gave no verdict" }), branch({ id: "b-perf", head: "performance", status: "cancelled", failure_reason: "stopped: branch security failed" })],
    );
    expect(column("security").getByRole("alert").textContent).toBe("reviewer gave no verdict");
    const stopped = column("performance").getByText("stopped: branch security failed");
    expect(column("performance").queryByRole("alert")).toBeNull();
    expect(stopped.getAttribute("role")).toBeNull();
    expect(stopped.className).toContain("text-zinc-500");
    expect(stopped.className).not.toContain("bg-red-50");
  });

  it("shows the fork's own reason above the columns, as a node card shows its reason", () => {
    renderRun(
      [fork({ status: "failed", failure_reason: "no matching edge from panel" }), visit("n-sec", "security", "b-sec")],
      two,
    );
    const alert = within(screen.getByRole("region", { name: "Fork panel" })).getByRole("alert");
    expect(alert.textContent).toBe("no matching edge from panel");
    expect(alert.className).toContain("bg-red-50");
    expect(before(alert, screen.getByRole("region", { name: "Branch security" }))).toBe(true);
    // it belongs to the fork, not to a column
    expect(column("security").queryByText("no matching edge from panel")).toBeNull();
    cleanup();
    renderRun([fork(), visit("n-sec", "security", "b-sec")], two);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("stacks the columns below the large breakpoint and puts them side by side from it", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec")], two);
    const grid = screen.getByRole("region", { name: "Branch security" }).parentElement as HTMLElement;
    expect(grid.className).toContain("grid");
    expect(grid.className).toContain("lg:grid-cols-2");
    expect(grid.className).not.toContain("md:grid-cols");
    expect(grid.className).not.toMatch(/(^| )grid-cols-/);
  });

  it("gives each of three branches a column, and four or five the extra large breakpoint", () => {
    const three = [...two, branch({ id: "b-main", head: "maintainability" })];
    renderRun([fork({ output: { branches: ["security", "performance", "maintainability"] } })], three);
    expect((screen.getByRole("region", { name: "Branch security" }).parentElement as HTMLElement).className).toContain("lg:grid-cols-3");
    cleanup();
    const four = ["a", "b", "c", "d"].map((head) => branch({ id: `b-${head}`, head }));
    renderRun([fork({ output: { branches: ["a", "b", "c", "d"] } })], four);
    const grid4 = (screen.getByRole("region", { name: "Branch a" }).parentElement as HTMLElement).className;
    expect(grid4).toContain("xl:grid-cols-4");
    expect(grid4).not.toContain("lg:grid-cols");
    cleanup();
    const five = ["a", "b", "c", "d", "e"].map((head) => branch({ id: `b-${head}`, head }));
    renderRun([fork({ output: { branches: ["a", "b", "c", "d", "e"] } })], five);
    const grid5 = (screen.getByRole("region", { name: "Branch a" }).parentElement as HTMLElement).className;
    expect(grid5).toContain("xl:grid-cols-5");
    expect(grid5).not.toContain("lg:grid-cols");
  });

  it("says a branch has not started a step yet, and that an ended one recorded none", () => {
    renderRun([fork({ status: "running", finished_at: undefined })], [branch({ status: "running", finished_at: undefined }), branch({ id: "b-perf", head: "performance", status: "succeeded" })]);
    expect(column("security").getByText("Starting…")).not.toBeNull();
    expect(column("performance").getByText("No activity recorded.")).not.toBeNull();
  });

  it("lists the model calls of a branch's visit in its card", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), visit("n-perf", "performance", "b-perf")], two, { model_calls: [call()] });
    expect(within(screen.getByRole("region", { name: "Node security, branch security" })).getByText("claude-sonnet-5-5")).not.toBeNull();
    expect(within(screen.getByRole("region", { name: "Node performance, branch performance" })).queryByText("claude-sonnet-5-5")).toBeNull();
  });

  it("follows the fork card with a short join card holding the time", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), visit("n-perf", "performance", "b-perf"), join()], two);
    const card = within(screen.getByRole("region", { name: "Join merge" }));
    expect(card.getByText("Joined 2 branches")).not.toBeNull();
    expect(card.getByRole("heading", { name: "merge" })).not.toBeNull();
    expect(card.getByText(formatDateTime(at(50)))).not.toBeNull();
    expect(before(screen.getByRole("region", { name: "Fork panel" }), screen.getByRole("region", { name: "Join merge" }))).toBe(true);
    // the join is a card of its own in the timeline, not part of the fork card
    expect(within(screen.getByRole("region", { name: "Fork panel" })).queryByText("Joined 2 branches")).toBeNull();
  });

  it("counts one branch in the singular and shows no join card before the join", () => {
    renderRun([fork({ output: { branches: ["security"] } }), join({ output: { branches: ["security"] } })], [branch()]);
    expect(screen.getByText("Joined 1 branch")).not.toBeNull();
    cleanup();
    renderRun([fork({ status: "running", finished_at: undefined })], two);
    expect(screen.queryByText(/^Joined/)).toBeNull();
  });

  it("is a list item of the timeline, and the join is the next one", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), join()], two);
    // the first list in the document is the timeline's own; the columns hold lists of their own
    const items = Array.from(document.body.querySelector("ol")!.children);
    expect(items.map((li) => li.querySelector("section")?.getAttribute("aria-label"))).toEqual(["Fork panel", "Join merge"]);
  });

  it("numbers the second round of a loop around the pair and lists its own branches", () => {
    renderRun(
      [
        fork(),
        visit("n-sec", "security", "b-sec"),
        join(),
        fork({ id: "f2", visit: 2, status: "running", started_at: at(60), finished_at: undefined }),
        visit("n-sec-2", "security", "b-sec-2", { visit: 2, started_at: at(61), status: "running", finished_at: undefined }),
      ],
      [...two, branch({ id: "b-sec-2", fork_visit_id: "f2", started_at: at(60), status: "running", finished_at: undefined }), branch({ id: "b-perf-2", fork_visit_id: "f2", head: "performance", started_at: at(60), status: "running", finished_at: undefined })],
    );
    expect(screen.getByRole("region", { name: "Fork panel, visit 1" })).not.toBeNull();
    expect(screen.getByRole("region", { name: "Fork panel, visit 2" })).not.toBeNull();
    expect(screen.getByRole("region", { name: "Node security, visit 2, branch security" })).not.toBeNull();
    expect(screen.getByRole("region", { name: "Node security, visit 1, branch security" })).not.toBeNull();
  });

  it("names the columns and the join with the fork visit's number when a loop around the pair ran it more than once", () => {
    renderRun(
      [
        fork(),
        visit("n-sec", "security", "b-sec"),
        visit("n-perf", "performance", "b-perf"),
        join(),
        fork({ id: "f2", visit: 2, started_at: at(60), finished_at: at(90) }),
        visit("n-sec-2", "security", "b-sec-2", { visit: 2, started_at: at(61), finished_at: at(80) }),
        visit("n-perf-2", "performance", "b-perf-2", { visit: 2, started_at: at(61), finished_at: at(80) }),
        join({ id: "j2", visit: 2, started_at: at(90), finished_at: at(90) }),
      ],
      [
        ...two,
        branch({ id: "b-sec-2", fork_visit_id: "f2", started_at: at(60) }),
        branch({ id: "b-perf-2", fork_visit_id: "f2", head: "performance", started_at: at(60) }),
      ],
    );
    const names = (re: RegExp) => screen.getAllByRole("region", { name: re }).map((el) => el.getAttribute("aria-label"));
    expect(names(/^Branch /)).toEqual(["Branch security, visit 1", "Branch performance, visit 1", "Branch security, visit 2", "Branch performance, visit 2"]);
    expect(names(/^Join /)).toEqual(["Join merge, visit 1", "Join merge, visit 2"]);
    expect(names(/^Fork /)).toEqual(["Fork panel, visit 1", "Fork panel, visit 2"]);
    // each name leads to its own column
    expect(within(screen.getByRole("region", { name: "Branch security, visit 2" })).getByRole("region", { name: "Node security, visit 2, branch security" })).not.toBeNull();
  });

  it("leaves the names without a number when the fork ran once", () => {
    renderRun([fork(), visit("n-sec", "security", "b-sec"), join()], two);
    expect(screen.getAllByRole("region", { name: /^(Branch|Join) / }).map((el) => el.getAttribute("aria-label"))).toEqual(["Branch security", "Branch performance", "Join merge"]);
  });

  it("leaves a run without branches as it was", () => {
    renderRun([visit("n1", "coder", "")], []);
    expect(screen.queryByRole("region", { name: /^Fork/ })).toBeNull();
    expect(screen.getAllByRole("region", { name: /^Node / })).toHaveLength(1);
  });
});
