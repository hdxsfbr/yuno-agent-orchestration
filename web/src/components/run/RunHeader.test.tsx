// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Run, RunStatus, ToolCall } from "@/lib/api";
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

function renderHeader(r: Run = run(), toolCalls: ToolCall[] = []) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <RunHeader run={r} connection="open" now={Date.parse("2026-10-15T10:06:00Z")} toolCalls={toolCalls} onRunChange={onRunChange} />
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
