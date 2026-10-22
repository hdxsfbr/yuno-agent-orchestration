// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Approval, Run, RunBranch, RunStatus, ToolCall } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, projectSettings, run } from "@/test/fixtures";
import RunHeader from "./RunHeader";

let api: ReturnType<typeof installFakeApi>;
let onRunChange: ReturnType<typeof vi.fn<(run: Run) => void>>;

beforeEach(() => {
  api = installFakeApi();
  onRunChange = vi.fn<(run: Run) => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderHeader(r: Run = run(), toolCalls: ToolCall[] = [], approvals: Approval[] = []) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <RunHeader run={r} connection="open" now={Date.parse("2026-10-15T10:06:00Z")} toolCalls={toolCalls} approvals={approvals} onRunChange={onRunChange} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/** Holds the run the way the page does: what the header hands over (or the stream delivers) replaces it. */
let apply: (r: Run) => void;

function LiveHeader({ initial }: { initial: Run }) {
  const [current, setCurrent] = useState(initial);
  apply = (next) => setCurrent(next);
  return (
    <RunHeader
      run={current}
      connection="open"
      now={Date.parse("2026-10-15T10:06:00Z")}
      onRunChange={(next) => {
        onRunChange(next);
        setCurrent(next);
      }}
    />
  );
}

function renderLive(initial: Run) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <LiveHeader initial={initial} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const cancelButton = () => screen.getByRole("button", { name: /^Cancel (run|requested)$/ }) as HTMLButtonElement;
const retryButton = () => screen.queryByRole("button", { name: "Retry" }) as HTMLButtonElement | null;
const header = () => within(screen.getByRole("region", { name: "Run summary" }));

describe("a ship run's trigger", () => {
  it("names the feature and wave and links to the project's plans", async () => {
    api.on("GET", "/api/plans/pl1", () => ({ plan: { id: "pl1", project_id: "p1", feature: "Health endpoint" }, tickets: [], events: [] }));
    renderHeader(run({ trigger: "plan", trigger_ref: "plan:pl1:wave:2", project_id: "p1" }));
    const link = await screen.findByRole("link", { name: "Feature Health endpoint, wave 2" });
    expect(link.getAttribute("href")).toBe("/projects/p1#project-plans");
  });

  it("shows the wave while the plan is not read", async () => {
    api.on("GET", "/api/plans/pl1", () => apiError(404, "not_found", "no such plan"));
    renderHeader(run({ trigger: "plan", trigger_ref: "plan:pl1:wave:2", project_id: "p1" }));
    const link = await screen.findByRole("link", { name: "Plan wave 2" });
    expect(link.getAttribute("href")).toBe("/projects/p1#project-plans");
  });
});

describe("the Retry button", () => {
  it.each<RunStatus>(["failed", "timed_out", "cancelled"])("is offered for a %s run that knows its step", (status) => {
    renderHeader(run({ status }));
    expect(retryButton()).not.toBeNull();
    expect(screen.queryByRole("button", { name: /Cancel/ })).toBeNull();
  });

  it.each<RunStatus>(["succeeded", "queued", "running", "waiting_approval"])("is not offered for a %s run", (status) => {
    renderHeader(run({ status, current_node: "coder" }));
    expect(retryButton()).toBeNull();
  });

  it("is not offered when the run has no step to retry from", () => {
    renderHeader(run({ status: "failed", current_node: undefined }));
    expect(retryButton()).toBeNull();
  });

  it("takes the place of Cancel: a live run offers Cancel and a finished one Retry", () => {
    renderHeader(run({ status: "running" }));
    expect(screen.getByRole("button", { name: "Cancel run" })).not.toBeNull();
    expect(retryButton()).toBeNull();
  });

  it("retries with one click, without a body, and hands over the run that comes back", async () => {
    const queued = run({ status: "queued", retries: 1, finished_at: undefined });
    api.on("POST", "/api/runs/r1/retry", () => queued);
    renderHeader();
    fireEvent.click(retryButton()!);
    await waitFor(() => expect(onRunChange).toHaveBeenCalledWith(queued));
    expect(api.made("POST", "/api/runs/r1/retry")).toHaveLength(1);
    expect(api.made("POST", "/api/runs/r1/retry")[0].body).toBeUndefined();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("is busy while the request is out", async () => {
    let release: (r: Run) => void = () => undefined;
    api.on("POST", "/api/runs/r1/retry", () => new Promise<Run>((r) => (release = r)));
    renderHeader();
    fireEvent.click(retryButton()!);
    const busy = (await screen.findByRole("button", { name: "Retrying…" })) as HTMLButtonElement;
    expect(busy.disabled).toBe(true);
    release(run({ status: "queued" }));
    await waitFor(() => expect(onRunChange).toHaveBeenCalled());
  });
});

describe("cancelling a run that is retried afterwards", () => {
  const cancelled = () => run({ status: "cancelled", cancel_requested: true });
  const queued = () => run({ status: "queued", retries: 1, cancel_requested: false, finished_at: undefined });

  async function cancelAndEnd() {
    api.on("POST", "/api/runs/r1/cancel", () => run({ status: "running", cancel_requested: true }));
    renderLive(run({ status: "running", finished_at: undefined }));
    fireEvent.click(cancelButton());
    await waitFor(() => expect(cancelButton().textContent).toBe("Cancel requested"));
    expect(cancelButton().disabled).toBe(true);
    act(() => apply(cancelled()));
    expect(screen.queryByRole("button", { name: /^Cancel/ })).toBeNull();
  }

  it("offers Cancel again once the cancelled run is retried and queued", async () => {
    await cancelAndEnd();
    api.on("POST", "/api/runs/r1/retry", queued);
    fireEvent.click(retryButton()!);
    await waitFor(() => expect(onRunChange).toHaveBeenLastCalledWith(expect.objectContaining({ status: "queued", retries: 1 })));
    expect(cancelButton().textContent).toBe("Cancel run");
    expect(cancelButton().disabled).toBe(false);
  });

  it("can cancel the retried run, and the request goes out again", async () => {
    await cancelAndEnd();
    api.on("POST", "/api/runs/r1/retry", queued);
    fireEvent.click(retryButton()!);
    await waitFor(() => expect(cancelButton().disabled).toBe(false));
    fireEvent.click(cancelButton());
    await waitFor(() => expect(api.made("POST", "/api/runs/r1/cancel")).toHaveLength(2));
  });

  it("offers Cancel again after a retry made from the limits dialog too", async () => {
    await cancelAndEnd();
    api.on("POST", "/api/runs/r1/retry", (r) => {
      if (r.body === undefined) return apiError(409, "limits_exhausted", "used up", [{ path: "max_steps", message: "40 of 40 used" }]);
      return queued();
    });
    fireEvent.click(retryButton()!);
    const d = within(await screen.findByRole("dialog"));
    fireEvent.change(d.getByLabelText(/^Max steps/), { target: { value: "80" } });
    fireEvent.click(d.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(cancelButton().textContent).toBe("Cancel run");
    expect(cancelButton().disabled).toBe(false);
  });
});

describe("a refused retry", () => {
  it.each([
    ["retry_limit", "run was retried 5 times"],
    ["run_not_retryable", "run is succeeded"],
  ])("shows the message of a 409 %s inline, without a dialog", async (code, message) => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(409, code, message));
    renderHeader();
    fireEvent.click(retryButton()!);
    expect((await screen.findByRole("alert")).textContent).toBe(`Could not retry the run: ${message}`);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onRunChange).not.toHaveBeenCalled();
    expect(retryButton()!.disabled).toBe(false);
  });

  it("shows any other refusal inline too", async () => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(429, "project_budget_reached", "project web has reached its daily budget ($5.00)"));
    renderHeader();
    fireEvent.click(retryButton()!);
    expect((await screen.findByRole("alert")).textContent).toContain("has reached its daily budget");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens the dialog for 409 limits_exhausted, listing the limits that are used up", async () => {
    api.on("POST", "/api/runs/r1/retry", () =>
      apiError(409, "limits_exhausted", "the run used up its limits", [
        { path: "max_steps", message: "40 of 40 used" },
        { path: "max_cost_usd", message: "$2.10 of $2.00 used" },
      ]),
    );
    renderHeader(run({ limits: { max_steps: 40, max_cost_usd: 2, max_tool_calls: 60 } }));
    fireEvent.click(retryButton()!);
    const d = within(await screen.findByRole("dialog"));
    const list = within(d.getByRole("list", { name: "Limits used up" }));
    expect(list.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Max steps: 40 of 40 used", "Max cost (USD): $2.10 of $2.00 used"]);
    expect(d.getByText("Raise the limits that are used up to retry from where the run stopped. A field left empty keeps its current value.")).not.toBeNull();
    // the refusal is not told twice
    expect(screen.queryByText(/Could not retry the run/)).toBeNull();
  });

  it("retries from the dialog and hands over the queued run", async () => {
    api.on("POST", "/api/runs/r1/retry", (r) => {
      if (r.body === undefined) return apiError(409, "limits_exhausted", "used up", [{ path: "max_steps", message: "40 of 40 used" }]);
      return run({ status: "queued", retries: 1 });
    });
    renderHeader(run({ limits: { max_steps: 40 } }));
    fireEvent.click(retryButton()!);
    const d = within(await screen.findByRole("dialog"));
    fireEvent.change(d.getByLabelText(/^Max steps/), { target: { value: "80" } });
    fireEvent.click(d.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(onRunChange).toHaveBeenCalledWith(expect.objectContaining({ status: "queued", retries: 1 })));
    expect(api.made("POST", "/api/runs/r1/retry")[1].body).toEqual({ limits: { max_steps: 80 } });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("closes the dialog on Cancel and offers Retry again without the old refusal", async () => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(409, "limits_exhausted", "used up", [{ path: "max_steps", message: "40 of 40 used" }]));
    renderHeader(run({ limits: { max_steps: 40 } }));
    fireEvent.click(retryButton()!);
    const d = within(await screen.findByRole("dialog"));
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByRole("alert")).toBeNull();
    expect(onRunChange).not.toHaveBeenCalled();
  });

  it("tells the dialog when the run already deployed a release or created a GitLab request", async () => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(409, "limits_exhausted", "used up", [{ path: "max_steps", message: "40 of 40 used" }]));
    const deploy = { id: "t1", run_id: "r1", run_node_id: "n1", tool_use_id: "u1", tool_name: "yuno__deploy_release", input: {}, is_error: false, status: "executed", attempt: 1, duration_ms: 1 } as ToolCall;
    renderHeader(run({ limits: { max_steps: 40 } }), [deploy]);
    fireEvent.click(retryButton()!);
    const d = within(await screen.findByRole("dialog"));
    expect(d.getByText(/already deployed a release or created a merge request through the GitLab API/)).not.toBeNull();
  });
});

describe("the Retried badge", () => {
  it("counts the retries", () => {
    renderHeader(run({ retries: 2 }));
    expect(header().getByText("Retried 2×")).not.toBeNull();
  });

  it("is absent for a run that was never retried", () => {
    renderHeader(run({ retries: 0 }));
    expect(header().queryByText(/Retried/)).toBeNull();
    cleanup();
    renderHeader(run({ retries: undefined }));
    expect(header().queryByText(/Retried/)).toBeNull();
  });
});

describe("the merge request", () => {
  it("has no link in the header any more: the card under it carries the request", () => {
    const call = {
      id: "t-mr",
      run_id: "r1",
      run_node_id: "n1",
      tool_use_id: "u2",
      tool_name: "yuno__open_merge_request",
      input: {},
      output: { number: 12, url: "https://gitlab.test/g/p/-/merge_requests/12", provider: "gitlab" },
      is_error: false,
      status: "executed",
      attempt: 1,
      duration_ms: 1,
    } as ToolCall;
    renderHeader(run({ status: "running" }), [call]);
    expect(header().queryByRole("link", { name: /request/i })).toBeNull();
  });
});

describe("a run that updates a merge request", () => {
  const updating = (over: Partial<Run> = {}) =>
    run({ status: "running", project_id: "p1", project: projectSettings(), merge_request_id: "mr1", merge_request: mergeRequest({ id: "mr1", run_id: "r0" }), ...over });
  const projectItem = () => header().getByText("Project").nextElementSibling as HTMLElement;

  it("says Updates merge request !12, linked to the provider, next to the project link", () => {
    renderHeader(updating());
    const link = within(projectItem()).getByRole("link", { name: "Updates merge request !12" });
    expect(link.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noreferrer");
    expect(within(projectItem()).getByRole("link", { name: "yuno-playground" }).getAttribute("href")).toBe("/projects/p1");
  });

  it("says Updates pull request #4 on GitHub", () => {
    renderHeader(updating({ project: projectSettings({ provider: "github" }), merge_request: mergeRequest({ id: "mr1", provider: "github", number: 4, url: "https://github.com/o/r/pull/4" }) }));
    const link = within(projectItem()).getByRole("link", { name: "Updates pull request #4" });
    expect(link.getAttribute("href")).toBe("https://github.com/o/r/pull/4");
  });

  it("says nothing for a run that opens its own request, or none", () => {
    renderHeader(run({ status: "running", project_id: "p1", project: projectSettings(), merge_request: mergeRequest({ run_id: "r1" }) }));
    expect(header().queryByText(/Updates/)).toBeNull();
    cleanup();
    renderHeader(run({ status: "running", project_id: "p1", project: projectSettings() }));
    expect(header().queryByText(/Updates/)).toBeNull();
  });

  it("says nothing when the record it names is not joined onto the run", () => {
    renderHeader(updating({ merge_request: undefined }));
    expect(header().queryByText(/Updates/)).toBeNull();
    cleanup();
    renderHeader(updating({ merge_request: mergeRequest({ id: "other" }) }));
    expect(header().queryByText(/Updates/)).toBeNull();
  });
});

describe("an issue run", () => {
  const issue = (over: Partial<Run> = {}) => run({ status: "running", trigger: "issue", trigger_ref: "p1#7", project_id: "p1", project: projectSettings(), ...over });
  const triggerItem = () => header().getByText("Trigger").nextElementSibling as HTMLElement;

  it("shows issue #7 in place of the trigger, linking to the issue on GitLab, and not the raw reference", () => {
    renderHeader(issue());
    const link = within(triggerItem()).getByRole("link", { name: "issue #7" });
    expect(link.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/issues/7");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(triggerItem().textContent).toBe("issue #7");
  });

  it("links to the issue on GitHub", () => {
    renderHeader(issue({ project: projectSettings({ provider: "github", repo_url: "https://github.com/acme/docs.git" }) }));
    expect(within(triggerItem()).getByRole("link", { name: "issue #7" }).getAttribute("href")).toBe("https://github.com/acme/docs/issues/7");
  });

  it("shows issue #7 without a link when the run kept no snapshot of its project", () => {
    renderHeader(issue({ project: undefined, project_id: undefined }));
    expect(triggerItem().textContent).toBe("issue #7");
    expect(within(triggerItem()).queryByRole("link")).toBeNull();
  });

  it("shows the trigger and the reference as they are when the reference carries no issue number", () => {
    renderHeader(issue({ trigger_ref: "p1" }));
    expect(triggerItem().textContent).toBe("issuep1");
    expect(within(triggerItem()).queryByRole("link")).toBeNull();
  });

  it("is not marked as delegated", () => {
    renderHeader(issue());
    expect(header().queryByText("Delegated")).toBeNull();
  });
});

describe("the line for a run that asks a question", () => {
  const question = (over: Partial<Approval> = {}): Approval => ({
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
  const waiting = () => run({ status: "waiting_approval" });
  const asks = () => screen.queryByRole("link", { name: /^Asks:/ });

  it("says what the run asks, linking to the approvals on the page", () => {
    renderHeader(waiting(), [], [question()]);
    const link = asks()!;
    expect(link.textContent).toBe("Asks: Which greeting language should I use?");
    expect(link.getAttribute("href")).toBe("#approvals");
    expect(header().getByRole("link", { name: /^Asks:/ })).toBe(link);
  });

  it("gives the first 120 characters of a long question and marks the cut", () => {
    const long = `${"Q".repeat(119)}XYZ and more words after the cut`;
    renderHeader(waiting(), [], [question({ message: long })]);
    expect(asks()!.textContent).toBe(`Asks: ${"Q".repeat(119)}X…`);
  });

  it("puts a question that spans lines on one", () => {
    renderHeader(waiting(), [], [question({ message: "Which language?\n\nEnglish or Spanish?" })]);
    expect(asks()!.textContent).toBe("Asks: Which language? English or Spanish?");
  });

  it("names the question that has waited longest when several are open", () => {
    renderHeader(waiting(), [], [question({ id: "q2", message: "Second?", requested_at: "2026-10-18T10:05:00Z" }), question({ id: "q1", message: "First?" })]);
    expect(asks()!.textContent).toBe("Asks: First?");
    expect(screen.getAllByRole("link", { name: /^Asks:/ })).toHaveLength(1);
  });

  it("is not there once the question is answered, declined or expired", () => {
    for (const q of [question({ status: "approved", answer: "Spanish" }), question({ status: "rejected" }), question({ status: "expired" })]) {
      renderHeader(waiting(), [], [q]);
      expect(asks()).toBeNull();
      cleanup();
    }
  });

  it("is not there while the run is not waiting, however the question stands", () => {
    for (const status of ["queued", "running", "succeeded", "failed", "cancelled", "timed_out"] as const) {
      renderHeader(run({ status, current_node: "coder" }), [], [question()]);
      expect(asks()).toBeNull();
      cleanup();
    }
  });

  it("is not there for a run that waits on another kind of approval, or when the header is not told the approvals", () => {
    renderHeader(waiting(), [], [question({ kind: "node", message: "Deploy?" }), question({ id: "t1", kind: "tool", message: "Open the merge request" })]);
    expect(asks()).toBeNull();
    cleanup();
    renderHeader(waiting());
    expect(asks()).toBeNull();
  });
});

describe("the step line inside a fork", () => {
  const branch = (over: Partial<RunBranch> = {}): RunBranch => ({
    id: "b1",
    run_id: "r1",
    fork_visit_id: "f1",
    fork_id: "panel",
    head: "security",
    status: "running",
    started_at: "2026-10-15T10:00:10Z",
    ...over,
  });
  const three = () => [branch(), branch({ id: "b2", head: "performance" }), branch({ id: "b3", head: "maintainability" })];
  const live = (over: Partial<Run> = {}) => run({ status: "running", current_node: "panel", finished_at: undefined, ...over });

  function renderBranches(r: Run, branches: RunBranch[]) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <RunHeader run={r} connection="open" now={Date.parse("2026-10-15T10:06:00Z")} branches={branches} onRunChange={onRunChange} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  const stepLine = () => screen.getByText("Current node").nextElementSibling as HTMLElement;

  it("names the fork and how many branches are running", () => {
    renderBranches(live(), three());
    expect(stepLine().textContent).toBe("panel (3 branches running)");
  });

  it("counts a branch that waits for an answer apart from the running ones, and not one that has ended", () => {
    renderBranches(live(), [branch({ status: "waiting_approval" }), branch({ id: "b2", head: "performance", status: "succeeded" }), branch({ id: "b3", head: "maintainability", status: "running" })]);
    expect(stepLine().textContent).toBe("panel (1 branch running, 1 waiting for an answer)");
  });

  it("says how many run and how many wait when several of each", () => {
    renderBranches(live(), [
      branch(),
      branch({ id: "b2", head: "performance" }),
      branch({ id: "b3", head: "maintainability", status: "waiting_approval" }),
      branch({ id: "b4", head: "style", status: "waiting_approval" }),
    ]);
    expect(stepLine().textContent).toBe("panel (2 branches running, 2 waiting for an answer)");
  });

  it("says only that branches wait for an answer when none runs", () => {
    renderBranches(live(), [branch({ status: "waiting_approval" }), branch({ id: "b2", head: "performance", status: "succeeded" })]);
    expect(stepLine().textContent).toBe("panel (1 waiting for an answer)");
    cleanup();
    renderBranches(live(), [branch({ status: "waiting_approval" }), branch({ id: "b2", head: "performance", status: "waiting_approval" })]);
    expect(stepLine().textContent).toBe("panel (2 waiting for an answer)");
  });

  it("says one branch in the singular", () => {
    renderBranches(live(), [branch(), branch({ id: "b2", head: "performance", status: "succeeded" })]);
    expect(stepLine().textContent).toBe("panel (1 branch running)");
  });

  it("counts only the branches of the newest visit when the pair has looped", () => {
    renderBranches(live(), [
      ...three().map((b) => ({ ...b, status: "succeeded" as const })),
      branch({ id: "b4", fork_visit_id: "f2", started_at: "2026-10-15T10:04:00Z" }),
      branch({ id: "b5", fork_visit_id: "f2", head: "performance", started_at: "2026-10-15T10:04:00Z" }),
    ]);
    expect(stepLine().textContent).toBe("panel (2 branches running)");
  });

  it("reads as today when the run is at another node, has no branches, or no branch is working", () => {
    renderBranches(live({ current_node: "lead" }), three());
    expect(stepLine().textContent).toBe("lead");
    cleanup();
    renderBranches(live(), []);
    expect(stepLine().textContent).toBe("panel");
    cleanup();
    renderBranches(live(), three().map((b) => ({ ...b, status: "succeeded" as const })));
    expect(stepLine().textContent).toBe("panel");
  });

  it("shows no step line once the run has ended", () => {
    renderBranches(run({ status: "failed", current_node: "panel" }), three());
    expect(screen.queryByText("Current node")).toBeNull();
  });

  it("keeps the question line when the run has released the fork to wait for an answer", () => {
    const q: Approval = {
      id: "q1",
      run_id: "r1",
      run_node_id: "n1",
      kind: "question",
      node_id: "security",
      message: "Which branch should I review?",
      status: "pending",
      requested_at: "2026-10-18T10:00:00Z",
      expires_at: "2026-10-19T10:00:00Z",
    };
    const client = new QueryClient();
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <RunHeader run={live({ status: "waiting_approval" })} connection="open" now={Date.parse("2026-10-15T10:06:00Z")} approvals={[q]} branches={[branch({ status: "waiting_approval" }), branch({ id: "b2", head: "performance", status: "succeeded" })]} onRunChange={onRunChange} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(stepLine().textContent).toBe("panel (1 waiting for an answer)");
    expect(screen.getByRole("link", { name: "Asks: Which branch should I review?" })).not.toBeNull();
  });
});

describe("pause, resume and steer (DESIGN W2)", () => {
  const live = () => run({ status: "running", finished_at: undefined });

  it("pauses a live run and says the pause waits for the step to stop", async () => {
    api.on("POST", "/api/runs/r1/pause", () => live());
    renderHeader(live());
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await waitFor(() => expect(api.made("POST", "/api/runs/r1/pause")).toHaveLength(1));
    expect(await screen.findByText("Pause requested: the run pauses once its step stops.")).not.toBeNull();
    expect(onRunChange).toHaveBeenCalled();
  });

  it("offers Resume with an optional note on a paused run, and sends the note", async () => {
    const queued = run({ status: "queued", finished_at: undefined });
    api.on("POST", "/api/runs/r1/resume", () => queued);
    renderHeader(run({ status: "paused", finished_at: undefined }));
    expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Note for the step" }), { target: { value: "  use the staging branch  " } });
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(onRunChange).toHaveBeenCalledWith(queued));
    expect(api.made("POST", "/api/runs/r1/resume")[0].body).toEqual({ note: "use the staging branch" });
  });

  it("resumes without a body when there is no note", async () => {
    api.on("POST", "/api/runs/r1/resume", () => run({ status: "queued", finished_at: undefined }));
    renderHeader(run({ status: "paused", finished_at: undefined }));
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(api.made("POST", "/api/runs/r1/resume")).toHaveLength(1));
    expect(api.made("POST", "/api/runs/r1/resume")[0].body).toBeUndefined();
  });

  it("steers a live run with the message typed", async () => {
    api.on("POST", "/api/runs/r1/steer", () => live());
    renderHeader(live());
    fireEvent.click(screen.getByRole("button", { name: "Steer" }));
    const send = screen.getByRole("button", { name: "Send" }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Message to the running step" }), { target: { value: "Skip the docs." } });
    fireEvent.click(send);
    await waitFor(() => expect(api.made("POST", "/api/runs/r1/steer")).toHaveLength(1));
    expect(api.made("POST", "/api/runs/r1/steer")[0].body).toEqual({ message: "Skip the docs." });
    expect(await screen.findByText("Sent to the running step.")).not.toBeNull();
  });

  it("says why a refused command was refused", async () => {
    api.on("POST", "/api/runs/r1/steer", () => apiError(409, "command_rejected", "nothing is running; retry or resume with a note"));
    renderHeader(live());
    fireEvent.click(screen.getByRole("button", { name: "Steer" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Message to the running step" }), { target: { value: "Hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText(/Could not send the message: nothing is running; retry or resume with a note/)).not.toBeNull();
  });

  it("offers neither Pause nor Steer on a finished run, nor once a cancel was asked for", () => {
    renderHeader(run({ status: "failed" }));
    expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Steer" })).toBeNull();
    cleanup();
    renderHeader(run({ status: "running", cancel_requested: true, finished_at: undefined }));
    expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Steer" })).toBeNull();
  });
});
