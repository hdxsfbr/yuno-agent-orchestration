// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { RunNode } from "@/lib/api";
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
