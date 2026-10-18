// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Approval } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import ApprovalCard from "./ApprovalCard";

let api: ReturnType<typeof installFakeApi>;
let onDecided: ReturnType<typeof vi.fn<(a: Approval) => void>>;
let onStale: ReturnType<typeof vi.fn<() => void>>;

const question = (over: Partial<Approval> = {}): Approval => ({
  id: "q1",
  run_id: "r1",
  run_node_id: "rn1",
  kind: "question",
  node_id: "coder",
  message: "Which greeting language should I use?",
  options: ["English", "Spanish", "Portuguese"],
  status: "pending",
  requested_at: "2026-10-18T10:00:00Z",
  expires_at: "2026-10-19T10:00:00Z",
  ...over,
});

beforeEach(() => {
  api = installFakeApi();
  onDecided = vi.fn<(a: Approval) => void>();
  onStale = vi.fn<() => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderCard(approval: Approval, props: { showRun?: boolean } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ApprovalCard approval={approval} onDecided={onDecided} onStale={onStale} {...props} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return within(screen.getByRole("article", { name: `Approval ${shortId(approval.id)}` }));
}

const answered = (over: Partial<Approval> = {}) => question({ status: "approved", answer: "Spanish", decided_at: "2026-10-18T10:05:00Z", decided_by: "api", ...over });
const button = (card: ReturnType<typeof renderCard>, name: string) => card.getByRole("button", { name }) as HTMLButtonElement;

describe("a pending question", () => {
  it("is labelled a Question and shows the question as written, with when it expires", () => {
    const card = renderCard(question({ message: "Which language?\n  English or Spanish?" }));
    expect(card.getByText("Question")).not.toBeNull();
    expect(card.getByText("pending")).not.toBeNull();
    const text = card.getByText(/Which language\?/);
    expect(text.className).toContain("whitespace-pre-wrap");
    expect(text.textContent).toBe("Which language?\n  English or Spanish?");
    expect(card.getByText("Expires").nextElementSibling!.textContent).toBe(formatDateTime("2026-10-19T10:00:00Z"));
    expect(card.getByText("coder")).not.toBeNull();
  });

  it("offers one button for each suggested answer, a box for another answer, and Decline", () => {
    const card = renderCard(question());
    const suggested = within(card.getByRole("group", { name: "Suggested answers" }));
    expect(suggested.getAllByRole("button").map((b) => b.textContent)).toEqual(["English", "Spanish", "Portuguese"]);
    expect(card.getByLabelText("Your answer")).not.toBeNull();
    expect(button(card, "Answer").disabled).toBe(true);
    expect(card.getByLabelText("Reason")).not.toBeNull();
    expect(button(card, "Decline")).not.toBeNull();
  });

  it("does not offer Approve, Approve always or Reject: a question needs an answer", () => {
    const card = renderCard(question());
    for (const name of ["Approve", "Approve always", "Reject"]) expect(card.queryByRole("button", { name })).toBeNull();
  });

  it("offers the box and Decline alone when the agent suggested nothing", () => {
    const card = renderCard(question({ options: undefined }));
    expect(card.queryByRole("group", { name: "Suggested answers" })).toBeNull();
    expect(card.getAllByRole("button").map((b) => b.textContent)).toEqual(["Answer", "Decline"]);
    expect(card.getByLabelText("Your answer").getAttribute("placeholder")).toBe("Your answer");
  });

  it("keeps Answer disabled while the box is empty or only blanks, and enables it once there is text", () => {
    const card = renderCard(question());
    const box = card.getByLabelText("Your answer");
    fireEvent.change(box, { target: { value: "   \n " } });
    expect(button(card, "Answer").disabled).toBe(true);
    fireEvent.change(box, { target: { value: "Italian" } });
    expect(button(card, "Answer").disabled).toBe(false);
    fireEvent.change(box, { target: { value: "" } });
    expect(button(card, "Answer").disabled).toBe(true);
  });

  it("limits the box to 4096 characters", () => {
    const card = renderCard(question());
    expect((card.getByLabelText("Your answer") as HTMLTextAreaElement).maxLength).toBe(4096);
  });

  it("is a different card from a command approval: no box and no Decline there", () => {
    const card = renderCard({ ...question(), kind: "exec", message: "rm -rf build/", options: undefined });
    expect(card.queryByLabelText("Your answer")).toBeNull();
    expect(card.queryByRole("button", { name: "Decline" })).toBeNull();
    expect(card.getByRole("button", { name: "Approve" })).not.toBeNull();
  });
});

describe("answering", () => {
  it("sends the option that was clicked, as it is written, and hands over the decided record", async () => {
    const decided = answered();
    api.on("POST", "/api/approvals/q1/answer", () => decided);
    const card = renderCard(question());
    fireEvent.click(card.getByRole("button", { name: "Spanish" }));
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1));
    expect(onDecided).toHaveBeenCalledWith(decided);
    expect(api.made("POST", "/api/approvals/q1/answer")).toHaveLength(1);
    expect(api.made("POST", "/api/approvals/q1/answer")[0].body).toEqual({ answer: "Spanish" });
    expect(api.made("POST", /\/(approve|reject)$/)).toHaveLength(0);
  });

  it("sends what was typed, without the blanks around it", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => answered({ answer: "Italian" }));
    const card = renderCard(question());
    fireEvent.change(card.getByLabelText("Your answer"), { target: { value: "  Italian, please\nwith accents  " } });
    fireEvent.click(button(card, "Answer"));
    await waitFor(() => expect(onDecided).toHaveBeenCalled());
    expect(api.made("POST", "/api/approvals/q1/answer")[0].body).toEqual({ answer: "Italian, please\nwith accents" });
  });

  it("disables every button while the answer is on its way", async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((r) => (release = r));
    api.on("POST", "/api/approvals/q1/answer", async () => {
      await gate;
      return answered();
    });
    const card = renderCard(question());
    fireEvent.change(card.getByLabelText("Your answer"), { target: { value: "Italian" } });
    fireEvent.click(button(card, "Answer"));
    await waitFor(() => expect(button(card, "Decline").disabled).toBe(true));
    for (const b of card.getAllByRole("button")) expect((b as HTMLButtonElement).disabled).toBe(true);
    release();
    await waitFor(() => expect(onDecided).toHaveBeenCalled());
  });

  it("shows what the server found wrong with the answer next to the box, and keeps the card open", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => apiError(422, "validation_failed", "invalid request", [{ path: "answer", message: "1 to 4096 characters" }]));
    const card = renderCard(question());
    const box = card.getByLabelText("Your answer");
    fireEvent.change(box, { target: { value: "é".repeat(3000) } });
    fireEvent.click(button(card, "Answer"));
    expect(await card.findByText("1 to 4096 characters")).not.toBeNull();
    expect(card.getByText("1 to 4096 characters").closest("[role=alert]")).not.toBeNull();
    expect(box.getAttribute("aria-invalid")).toBe("true");
    expect(card.queryByText(/Could not record the decision/)).toBeNull();
    expect(onDecided).not.toHaveBeenCalled();
    // nothing is lost: the text is still there and can be sent again
    expect((box as HTMLTextAreaElement).value).toBe("é".repeat(3000));
    expect(button(card, "Answer").disabled).toBe(false);
    expect(button(card, "Decline").disabled).toBe(false);
  });

  it("shows a problem the server puts on another field with the field named", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => apiError(422, "validation_failed", "invalid request body", [{ path: "body", message: "unknown field \"reply\"" }]));
    const card = renderCard(question());
    fireEvent.click(card.getByRole("button", { name: "English" }));
    expect(await card.findByText('body: unknown field "reply"')).not.toBeNull();
    expect(card.queryByText(/Could not record the decision/)).toBeNull();
  });

  it("takes the message away when the next attempt is made", async () => {
    let refuse = true;
    api.on("POST", "/api/approvals/q1/answer", () => (refuse ? apiError(422, "validation_failed", "invalid request", [{ path: "answer", message: "1 to 4096 characters" }]) : answered()));
    const card = renderCard(question());
    fireEvent.change(card.getByLabelText("Your answer"), { target: { value: "long" } });
    fireEvent.click(button(card, "Answer"));
    await card.findByText("1 to 4096 characters");
    refuse = false;
    fireEvent.click(card.getByRole("button", { name: "English" }));
    await waitFor(() => expect(card.queryByText("1 to 4096 characters")).toBeNull());
    await waitFor(() => expect(onDecided).toHaveBeenCalled());
  });

  it("says the question was decided already on a 409, takes the controls away and tells the page", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => apiError(409, "not_pending", "approval is already approved"));
    const card = renderCard(question());
    fireEvent.click(card.getByRole("button", { name: "English" }));
    expect(await card.findByText("This approval was already decided.")).not.toBeNull();
    expect(card.queryByRole("button")).toBeNull();
    expect(card.queryByLabelText("Your answer")).toBeNull();
    expect(onStale).toHaveBeenCalledTimes(1);
    expect(onDecided).not.toHaveBeenCalled();
  });

  it("tells any other failure and keeps the controls", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => apiError(503, "unavailable", "the engine is not ready"));
    const card = renderCard(question());
    fireEvent.click(card.getByRole("button", { name: "English" }));
    expect(await card.findByText(/Could not record the decision: the engine is not ready/)).not.toBeNull();
    expect(card.getByRole("button", { name: "English" })).not.toBeNull();
    expect(onStale).not.toHaveBeenCalled();
  });

  it("tells a 422 that names nothing about the answer as a failure, with the server's message", async () => {
    api.on("POST", "/api/approvals/q1/answer", () => apiError(422, "validation_failed", "answer: only a question can be answered"));
    const card = renderCard(question());
    fireEvent.click(card.getByRole("button", { name: "English" }));
    expect(await card.findByText(/Could not record the decision: answer: only a question can be answered/)).not.toBeNull();
  });
});

describe("declining", () => {
  it("sends a decline with the reason typed", async () => {
    const declined = question({ status: "rejected", reason: "not now", decided_at: "2026-10-18T10:05:00Z", decided_by: "api" });
    api.on("POST", "/api/approvals/q1/reject", () => declined);
    const card = renderCard(question());
    fireEvent.change(card.getByLabelText("Reason"), { target: { value: "  not now  " } });
    fireEvent.click(button(card, "Decline"));
    await waitFor(() => expect(onDecided).toHaveBeenCalledWith(declined));
    expect(api.made("POST", "/api/approvals/q1/reject")[0].body).toEqual({ reason: "not now" });
    expect(api.made("POST", /\/answer$/)).toHaveLength(0);
  });

  it("sends no reason when none is typed", async () => {
    api.on("POST", "/api/approvals/q1/reject", () => question({ status: "rejected" }));
    const card = renderCard(question());
    fireEvent.click(button(card, "Decline"));
    await waitFor(() => expect(onDecided).toHaveBeenCalled());
    expect(api.made("POST", "/api/approvals/q1/reject")[0].body).toEqual({});
  });

  it("says the question was decided already on a 409", async () => {
    api.on("POST", "/api/approvals/q1/reject", () => apiError(409, "not_pending", "approval is already approved"));
    const card = renderCard(question());
    fireEvent.click(button(card, "Decline"));
    expect(await card.findByText("This approval was already decided.")).not.toBeNull();
    expect(card.queryByRole("button")).toBeNull();
    expect(onStale).toHaveBeenCalledTimes(1);
  });
});

describe("a question that was dealt with", () => {
  it("shows the answer, who gave it and when, and no controls", () => {
    const card = renderCard(answered({ decided_by: "chat:Chief of Staff" }));
    expect(card.getByText("Answered: Spanish")).not.toBeNull();
    expect(card.getByText("approved")).not.toBeNull();
    expect(card.getByText(`Decided ${formatDateTime("2026-10-18T10:05:00Z")} by chat:Chief of Staff`)).not.toBeNull();
    expect(card.queryByRole("button")).toBeNull();
    expect(card.queryByLabelText("Your answer")).toBeNull();
    expect(card.queryByText("Expires")).toBeNull();
  });

  it("keeps the lines of a long answer", () => {
    const card = renderCard(answered({ answer: "Use Spanish.\nAccents too." }));
    const line = card.getByText(/^Answered: Use Spanish/);
    expect(line.className).toContain("whitespace-pre-wrap");
    expect(line.textContent).toBe("Answered: Use Spanish.\nAccents too.");
  });

  it("shows a decline with its reason once", () => {
    const card = renderCard(question({ status: "rejected", reason: "not now", decided_at: "2026-10-18T10:05:00Z", decided_by: "api" }));
    expect(card.getByText("Declined: not now")).not.toBeNull();
    expect(card.getByText(`Decided ${formatDateTime("2026-10-18T10:05:00Z")} by api`)).not.toBeNull();
    expect(card.getAllByText(/not now/)).toHaveLength(1);
    expect(card.queryByRole("button")).toBeNull();
  });

  it("shows a decline without a reason as Declined", () => {
    const card = renderCard(question({ status: "rejected", decided_at: "2026-10-18T10:05:00Z", decided_by: "api" }));
    expect(card.getByText("Declined")).not.toBeNull();
  });

  it("says an expired question was not answered in time", () => {
    const card = renderCard(question({ status: "expired" }));
    expect(card.getByText("Not answered in time")).not.toBeNull();
    expect(card.getByText("expired")).not.toBeNull();
    expect(card.getByText("Expired").nextElementSibling!.textContent).toBe(formatDateTime("2026-10-19T10:00:00Z"));
    expect(card.queryByRole("button")).toBeNull();
  });

  it("keeps the reason a question expired with", () => {
    const card = renderCard(question({ status: "expired", reason: "run cancelled" }));
    expect(card.getByText("Not answered in time")).not.toBeNull();
    expect(card.getByText(/run cancelled/)).not.toBeNull();
  });

  it("says nothing of answers for other kinds of approval", () => {
    const card = renderCard({ ...question(), kind: "node", options: undefined, message: "Deploy?", status: "approved", decided_at: "2026-10-18T10:05:00Z", decided_by: "api", reason: "go" });
    expect(card.queryByText(/Answered|Declined|Not answered/)).toBeNull();
    expect(card.getByText(/by api · go/)).not.toBeNull();
  });
});
