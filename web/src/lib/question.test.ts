import { describe, expect, it } from "vitest";
import type { Approval } from "./api";
import { isQuestion, pendingQuestion, questionLine, questionOutcome } from "./question";

const approval = (over: Partial<Approval> = {}): Approval => ({
  id: "q1",
  run_id: "r1",
  run_node_id: "rn1",
  kind: "question",
  node_id: "coder",
  message: "Which greeting language should I use?",
  status: "pending",
  requested_at: "2026-10-18T10:00:00Z",
  expires_at: "2026-10-19T10:00:00Z",
  ...over,
});

describe("isQuestion", () => {
  it("is true for the question kind only", () => {
    expect(isQuestion(approval())).toBe(true);
    for (const kind of ["exec", "tool", "node"] as const) expect(isQuestion(approval({ kind }))).toBe(false);
  });
});

describe("questionOutcome", () => {
  it("is the answer for an answered question", () => {
    expect(questionOutcome(approval({ status: "approved", answer: "Portuguese" }))).toBe("Answered: Portuguese");
    expect(questionOutcome(approval({ status: "approved" }))).toBe("Answered");
  });

  it("is the decline, with the operator's reason when there is one", () => {
    expect(questionOutcome(approval({ status: "rejected", reason: "not now" }))).toBe("Declined: not now");
    expect(questionOutcome(approval({ status: "rejected" }))).toBe("Declined");
  });

  it("says nobody answered in time for an expired question, whatever the reason for the expiry", () => {
    expect(questionOutcome(approval({ status: "expired" }))).toBe("Not answered in time");
    expect(questionOutcome(approval({ status: "expired", reason: "run cancelled" }))).toBe("Not answered in time");
  });

  it("has nothing to say for a pending question or for another kind of approval", () => {
    expect(questionOutcome(approval())).toBeUndefined();
    expect(questionOutcome(approval({ kind: "tool", status: "approved", answer: "x" }))).toBeUndefined();
    expect(questionOutcome(approval({ kind: "node", status: "rejected", reason: "no" }))).toBeUndefined();
  });
});

describe("pendingQuestion", () => {
  it("is the oldest question still waiting", () => {
    const newer = approval({ id: "q2", requested_at: "2026-10-18T10:05:00Z" });
    const older = approval({ id: "q1", requested_at: "2026-10-18T10:01:00Z" });
    expect(pendingQuestion([newer, older])?.id).toBe("q1");
  });

  it("skips decided questions and approvals of other kinds", () => {
    const list = [
      approval({ id: "done", status: "approved", answer: "x" }),
      approval({ id: "gate", kind: "node" }),
      approval({ id: "tool", kind: "tool" }),
      approval({ id: "q3", requested_at: "2026-10-18T10:09:00Z" }),
    ];
    expect(pendingQuestion(list)?.id).toBe("q3");
    expect(pendingQuestion(list.slice(0, 3))).toBeUndefined();
    expect(pendingQuestion([])).toBeUndefined();
  });
});

describe("questionLine", () => {
  it("keeps a short question as it is", () => {
    expect(questionLine("Which language?")).toBe("Which language?");
  });

  it("puts a question that spans lines on one", () => {
    expect(questionLine("  Which language?\n\nEnglish or   Spanish?  ")).toBe("Which language? English or Spanish?");
  });

  it("keeps exactly 120 characters and marks a longer question as cut", () => {
    const exact = "a".repeat(120);
    expect(questionLine(exact)).toBe(exact);
    expect(questionLine(`${exact}b`)).toBe(`${exact}…`);
    expect(questionLine("x".repeat(500))).toBe(`${"x".repeat(120)}…`);
  });

  it("counts characters, not UTF-16 units, so a cut never splits one", () => {
    const text = "🙂".repeat(130);
    expect(questionLine(text)).toBe(`${"🙂".repeat(120)}…`);
  });
});
