// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { Approval, RunNode } from "@/lib/api";
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
