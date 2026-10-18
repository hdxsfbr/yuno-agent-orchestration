import type { Approval } from "./api";

// What the run page and the inbox say about a question to the operator (DESIGN 5.24 rule 12).

export const isQuestion = (a: Pick<Approval, "kind">): boolean => a.kind === "question";

/**
 * The line a question that has been dealt with leaves on its card and in the timeline:
 * the answer, the decline with its reason, or that nobody answered in time. A pending
 * question and any other kind of approval have none.
 */
export function questionOutcome(a: Pick<Approval, "kind" | "status" | "answer" | "reason">): string | undefined {
  if (a.kind !== "question") return undefined;
  switch (a.status) {
    case "approved":
      return a.answer ? `Answered: ${a.answer}` : "Answered";
    case "rejected":
      return a.reason ? `Declined: ${a.reason}` : "Declined";
    case "expired":
      return "Not answered in time";
    default:
      return undefined;
  }
}

/** The question the run waits on: the oldest pending one, when there is one. */
export function pendingQuestion(approvals: readonly Approval[]): Approval | undefined {
  const at = (a: Approval) => Date.parse(a.requested_at) || 0;
  return approvals.filter((a) => a.kind === "question" && a.status === "pending").sort((a, b) => at(a) - at(b))[0];
}

/** The question on one line for a header: whitespace collapsed, cut after `max` characters with an ellipsis. */
export function questionLine(text: string, max = 120): string {
  const chars = Array.from(text.replace(/\s+/g, " ").trim());
  return chars.length > max ? `${chars.slice(0, max).join("")}…` : chars.join("");
}
