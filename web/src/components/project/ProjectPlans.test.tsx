// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { MergeRequest, Plan, PlanDetail, PlanEvent, PlanTicket, Project } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, project } from "@/test/fixtures";
import ProjectPlans from "./ProjectPlans";

let api: ReturnType<typeof installFakeApi>;
let current: Project;
let plans: Plan[];
let detail: PlanDetail;
let requests: MergeRequest[];
let pendingApprovals: Record<string, { id: string; node_id: string }[]>;

const plan = (over: Partial<Plan> = {}): Plan => ({
  id: "pl1",
  project_id: "p1",
  feature: "Health endpoint",
  plan_run_id: "run0",
  status: "building",
  current_wave: 2,
  created_at: "2026-10-19T09:00:00Z",
  updated_at: "2026-10-19T10:00:00Z",
  ...over,
});
const ticket = (over: Partial<PlanTicket> = {}): PlanTicket => ({
  plan_id: "pl1",
  ticket_id: "T1",
  wave: 1,
  title: "Add the route",
  issue_number: 7,
  issue_url: "https://gitlab.com/x/y/-/issues/7",
  status: "merged",
  updated_at: "2026-10-19T10:00:00Z",
  ...over,
});
const event = (over: Partial<PlanEvent> = {}): PlanEvent => ({ id: "e1", plan_id: "pl1", at: "2026-10-19T10:00:00Z", kind: "published", detail: {}, ...over });

beforeEach(() => {
  current = project();
  plans = [plan()];
  requests = [];
  pendingApprovals = {};
  detail = {
    plan: plan(),
    tickets: [
      ticket(),
      ticket({ ticket_id: "T2", wave: 2, title: "Add the test", issue_number: 8, issue_url: "https://gitlab.com/x/y/-/issues/8", status: "in_review", run_id: "run-12345678", merge_request_id: "mr1" }),
    ],
    events: [event({ id: "e2", kind: "wave_released", wave: 2 }), event({ id: "e1" })],
  };
  api = installFakeApi();
  api.on("GET", "/api/projects/p1/plans", () => plans);
  api.on("GET", "/api/plans/pl1", () => detail);
  api.on("GET", /^\/api\/merge-requests\?/, () => requests);
  api.on("GET", /^\/api\/approvals\?status=pending&run_id=/, ({ url }) => pendingApprovals[new URL(url, "http://x").searchParams.get("run_id") ?? ""] ?? []);
  api.on("POST", "/api/projects/p1/waves/pause", ({ body }) => {
    current = { ...current, waves_paused: true, waves_paused_reason: (body as { reason: string }).reason };
    return current;
  });
  api.on("POST", "/api/projects/p1/waves/resume", () => {
    current = { ...current, waves_paused: false, waves_paused_reason: undefined };
    return current;
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPlans(p: Project = current) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const ui = (pr: Project) => (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ProjectPlans project={pr} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(ui(p));
  return { rerender: (pr: Project) => view.rerender(ui(pr)), client };
}

describe("the open plan", () => {
  it("shows the feature, status and current wave, with its tickets grouped by wave", async () => {
    renderPlans();
    expect(await screen.findByText("Health endpoint")).not.toBeNull();
    expect(screen.getAllByText("Building").length).toBeGreaterThan(0);
    expect(screen.getByText(/Current wave 2/)).not.toBeNull();
    expect(await screen.findByText("Wave 1")).not.toBeNull();
    expect(screen.getByText("Wave 2 · current")).not.toBeNull();
    expect(screen.getByText("Add the route")).not.toBeNull();
    expect(screen.getByRole("link", { name: "#7" }).getAttribute("href")).toBe("https://gitlab.com/x/y/-/issues/7");
    expect(screen.getByRole("link", { name: "12345678" }).getAttribute("href")).toBe("/runs/run-12345678");
    expect(screen.getByText("In review")).not.toBeNull();
  });

  it("links a ticket's request and marks a draft one", async () => {
    requests = [mergeRequest({ id: "mr1", draft: true })];
    renderPlans();
    expect((await screen.findByRole("link", { name: "!12" })).getAttribute("href")).toContain("merge_requests/12");
    expect(screen.getByText("Draft")).not.toBeNull();
  });

  it("shows no Draft badge for a request that is ready", async () => {
    requests = [mergeRequest({ id: "mr1", draft: false })];
    renderPlans();
    await screen.findByRole("link", { name: "!12" });
    expect(screen.queryByText("Draft")).toBeNull();
  });

  it("lists older plans apart from the open one", async () => {
    plans = [plan(), plan({ id: "pl0", feature: "Login page", status: "done" })];
    renderPlans();
    expect(await screen.findByText("Login page")).not.toBeNull();
    expect(screen.getByText("Older plans")).not.toBeNull();
    expect(screen.getAllByText("Done").length).toBeGreaterThan(0);
  });

  it("says so when no plan is open", async () => {
    plans = [];
    renderPlans();
    expect(await screen.findByText(/No plan is open/)).not.toBeNull();
  });
});

describe("the wave timeline", () => {
  const waveHeading = async (text: string) => (await screen.findByText(text)).parentElement as HTMLElement;

  it("shows each wave with its state, in order", async () => {
    detail.tickets = [
      ticket({ ticket_id: "T3", wave: 3, title: "Docs", status: "pending" }),
      ticket({ ticket_id: "T2", wave: 2, title: "Add the test", status: "released" }),
      ticket({ ticket_id: "T1", wave: 1, status: "merged" }),
    ];
    detail.ship_runs = [{ wave: 1, run_id: "ship-0001abcd", status: "succeeded" }];
    renderPlans();
    expect(within(await waveHeading("Wave 1")).getByText("Done")).not.toBeNull();
    expect(within(await waveHeading("Wave 2 · current")).getByText("Released")).not.toBeNull();
    expect(within(await waveHeading("Wave 3")).getByText("Pending")).not.toBeNull();
    const headings = screen.getAllByRole("heading", { level: 4 }).map((h) => h.textContent);
    expect(headings.slice(0, 3)).toEqual(["Wave 1Done", "Wave 2 · currentReleased", "Wave 3Pending"]);
  });

  it("calls a wave building once a ticket is being worked on", async () => {
    plans = [plan({ current_wave: 1 })];
    detail.plan = plan({ current_wave: 1 });
    detail.tickets = [ticket({ status: "building" })];
    renderPlans();
    expect(within(await waveHeading("Wave 1 · current")).getByText("Building")).not.toBeNull();
  });

  it("shows the ship run of a wave as shipping, with a link", async () => {
    plans = [plan({ status: "shipping", current_wave: 2 })];
    detail.plan = plan({ status: "shipping", current_wave: 2 });
    detail.ship_runs = [{ wave: 2, run_id: "ship-0002abcd", status: "running" }];
    renderPlans();
    expect(within(await waveHeading("Wave 2 · current")).getByText("Shipping")).not.toBeNull();
    expect(screen.getByRole("link", { name: "0002abcd" }).getAttribute("href")).toBe("/runs/ship-0002abcd");
    expect(screen.getByText("running")).not.toBeNull();
  });

  it("links the smoke test that is waiting to the ship run", async () => {
    plans = [plan({ status: "shipping", current_wave: 1 })];
    detail.plan = plan({ status: "shipping", current_wave: 1 });
    detail.tickets = [ticket(), ticket({ ticket_id: "T2", wave: 2, status: "pending" })];
    detail.ship_runs = [{ wave: 1, run_id: "ship-0001abcd", status: "waiting_approval" }];
    pendingApprovals["ship-0001abcd"] = [{ id: "a1", node_id: "smoke_test" }];
    renderPlans();
    const link = await screen.findByRole("link", { name: "Waiting for your smoke test" });
    expect(link.getAttribute("href")).toBe("/runs/ship-0001abcd");
    expect(api.made("GET", /^\/api\/approvals\?status=pending&run_id=ship-0001abcd$/)).toHaveLength(1);
  });

  it("does not call a tool approval of the ship run a smoke test", async () => {
    plans = [plan({ status: "shipping", current_wave: 1 })];
    detail.plan = plan({ status: "shipping", current_wave: 1 });
    detail.ship_runs = [{ wave: 1, run_id: "ship-0001abcd", status: "waiting_approval" }];
    pendingApprovals["ship-0001abcd"] = [{ id: "a1", node_id: "integrator" }];
    renderPlans();
    expect(await screen.findByRole("link", { name: "Waiting for your approval" })).not.toBeNull();
    expect(screen.queryByText("Waiting for your smoke test")).toBeNull();
  });

  it("asks for nothing while no ship run waits", async () => {
    plans = [plan({ status: "shipping", current_wave: 2 })];
    detail.plan = plan({ status: "shipping", current_wave: 2 });
    detail.ship_runs = [{ wave: 2, run_id: "ship-0002abcd", status: "running" }];
    renderPlans();
    await screen.findByText("Shipping");
    expect(api.made("GET", /^\/api\/approvals/)).toHaveLength(0);
  });

  it.each(["failed", "cancelled", "timed_out"] as const)("calls a wave held when its ship run %s, with the pause reason", async (status) => {
    plans = [plan({ status: "shipping", current_wave: 1 })];
    detail.plan = plan({ status: "shipping", current_wave: 1 });
    detail.ship_runs = [{ wave: 1, run_id: "ship-0001abcd", status }];
    current = project({ waves_paused: true, waves_paused_reason: "Smoke test failed" });
    renderPlans();
    expect(within(await waveHeading("Wave 1 · current")).getByText("Held")).not.toBeNull();
    expect(within(await waveHeading("Wave 1 · current")).queryByText("Shipping")).toBeNull();
    expect(screen.getByRole("link", { name: "0001abcd" }).getAttribute("href")).toBe("/runs/ship-0001abcd");
    expect(screen.getAllByText("Smoke test failed").length).toBeGreaterThan(0);
  });

  it("links the acceptance that is waiting after the last wave", async () => {
    plans = [plan({ status: "accepting", current_wave: 2 })];
    detail.plan = plan({ status: "accepting", current_wave: 2 });
    detail.ship_runs = [
      { wave: 1, run_id: "ship-0001abcd", status: "succeeded" },
      { wave: 2, run_id: "ship-0002abcd", status: "waiting_approval" },
    ];
    pendingApprovals["ship-0002abcd"] = [{ id: "a2", node_id: "accept_feature" }];
    renderPlans();
    const link = await screen.findByRole("link", { name: "Waiting for your acceptance" });
    expect(link.getAttribute("href")).toBe("/runs/ship-0002abcd");
    expect(screen.queryByText("Waiting for your smoke test")).toBeNull();
  });

  it.each([
    ["passed", "Passing"],
    ["failed", "Failing"],
    ["running", "Running"],
    ["pending", "Pending"],
    ["none", "No checks"],
  ] as const)("shows main as %s", async (state, label) => {
    detail.main = { state, sha: "0123456789abcdef", at: new Date(Date.now() - 5 * 60_000).toISOString(), url: "https://gitlab.com/x/y/-/pipelines/9" };
    renderPlans();
    const main = (await screen.findByText("Main")).parentElement as HTMLElement;
    expect(within(main).getByText(label)).not.toBeNull();
    expect(within(main).getByText("01234567")).not.toBeNull();
    expect(within(main).getByText("5 min ago")).not.toBeNull();
    expect(within(main).getByRole("link").getAttribute("href")).toBe("https://gitlab.com/x/y/-/pipelines/9");
  });

  it("shows main without a link when the provider gave no address", async () => {
    detail.main = { state: "passed", sha: "0123456789abcdef", at: "2026-10-19T10:00:00Z" };
    renderPlans();
    const main = (await screen.findByText("Main")).parentElement as HTMLElement;
    expect(within(main).queryByRole("link")).toBeNull();
  });

  it("says main has not been checked when there is no reading", async () => {
    detail.main = null;
    renderPlans();
    expect(await screen.findByText("Main has not been checked yet.")).not.toBeNull();
  });
});

describe("the events", () => {
  it("reads each kind as a sentence, newest first", async () => {
    detail.events = [
      event({ id: "e5", kind: "feature_done" }),
      event({ id: "e4", kind: "accept_waiting" }),
      event({ id: "e3", kind: "wave_done", wave: 1 }),
      event({ id: "e2", kind: "smoke_test_waiting", wave: 1 }),
      event({ id: "e1", kind: "resumed" }),
    ];
    renderPlans();
    const items = within((await screen.findByText("Events")).parentElement as HTMLElement).getAllByRole("listitem");
    expect(items.map((li) => li.textContent?.split(/\d{4}|Oct/)[0])).toEqual([
      "Feature done",
      "The feature is waiting for your acceptance",
      "Wave 1 done",
      "Wave 1 is waiting for your smoke test",
      "Waves resumed",
    ]);
  });

  it("reads a released wave", async () => {
    detail.events = [event({ id: "e1", kind: "wave_released", wave: 3 })];
    renderPlans();
    expect(await screen.findByText("Wave 3 released")).not.toBeNull();
  });

  it("shows the reason of an escalation", async () => {
    detail.events = [event({ id: "e9", kind: "escalated", ticket_id: "T2", detail: { reason: "escalated (prd_gap): the PRD does not say how login fails" } })];
    renderPlans();
    expect(await screen.findByText("Ticket T2 escalated")).not.toBeNull();
    expect(screen.getByText("escalated (prd_gap): the PRD does not say how login fails")).not.toBeNull();
  });
});

describe("pausing and resuming", () => {
  it("asks for a reason and pauses the waves with it", async () => {
    const view = renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Pause waves" }));
    const dialog = within(await screen.findByRole("dialog"));
    const submit = dialog.getByRole("button", { name: "Pause waves" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText(/Reason/), { target: { value: "Checking the schema" } });
    fireEvent.click(submit);
    await waitFor(() => expect(api.made("POST", "/api/projects/p1/waves/pause")).toHaveLength(1));
    expect(api.made("POST", "/api/projects/p1/waves/pause")[0].body).toEqual({ reason: "Checking the schema" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    view.rerender(current);
    expect((await screen.findAllByText("Paused")).length).toBeGreaterThan(0);
    expect(screen.getByText("Checking the schema")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Resume waves" })).not.toBeNull();
  });

  it("shows the server's message when the pause is refused", async () => {
    api.on("POST", "/api/projects/p1/waves/pause", () => apiError(422, "invalid", "reason is too long"));
    renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Pause waves" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText(/Reason/), { target: { value: "x" } });
    fireEvent.click(dialog.getByRole("button", { name: "Pause waves" }));
    expect((await dialog.findByRole("alert")).textContent).toBe("reason is too long");
  });

  it("shows the Paused badge with its reason, and resumes", async () => {
    current = project({ waves_paused: true, waves_paused_reason: "Smoke test failed" });
    const view = renderPlans();
    expect((await screen.findAllByText("Paused")).length).toBeGreaterThan(0);
    expect(screen.getByText("Smoke test failed")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Pause waves" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Resume waves" }));
    await waitFor(() => expect(api.made("POST", "/api/projects/p1/waves/resume")).toHaveLength(1));
    view.rerender(current);
    await waitFor(() => expect(screen.queryByText("Paused")).toBeNull());
    expect(screen.getByRole("button", { name: "Pause waves" })).not.toBeNull();
  });
});

describe("cancelling a plan", () => {
  it("asks for a reason and cancels the plan with it", async () => {
    api.on("POST", "/api/plans/pl1/cancel", ({ body }) => {
      plans = [plan({ status: "cancelled" })];
      return { ...plan({ status: "cancelled" }), reason: (body as { reason: string }).reason };
    });
    renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Cancel plan" }));
    const dialog = within(await screen.findByRole("dialog"));
    const submit = dialog.getByRole("button", { name: "Cancel plan" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText(/Reason/), { target: { value: "Wrong scope" } });
    fireEvent.click(submit);
    await waitFor(() => expect(api.made("POST", "/api/plans/pl1/cancel")).toHaveLength(1));
    expect(api.made("POST", "/api/plans/pl1/cancel")[0].body).toEqual({ reason: "Wrong scope" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("Cancelled")).not.toBeNull();
    expect(screen.getByText("Older plans")).not.toBeNull();
  });

  it("shows the server's message when the cancel is refused", async () => {
    api.on("POST", "/api/plans/pl1/cancel", () => apiError(409, "conflict", "the plan is already done"));
    renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Cancel plan" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText(/Reason/), { target: { value: "x" } });
    fireEvent.click(dialog.getByRole("button", { name: "Cancel plan" }));
    expect((await dialog.findByRole("alert")).textContent).toBe("the plan is already done");
  });

  it("shows a cancelled plan with a Cancelled badge among the older ones", async () => {
    plans = [plan({ id: "pl0", feature: "Login page", status: "cancelled" })];
    renderPlans();
    expect(await screen.findByText("Login page")).not.toBeNull();
    expect(screen.getByText("Cancelled")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel plan" })).toBeNull();
  });

  it("reads the cancelled and retried events", async () => {
    detail.events = [
      event({ id: "e2", kind: "retried", ticket_id: "T2" }),
      event({ id: "e1", kind: "cancelled", detail: { reason: "Wrong scope" } }),
    ];
    renderPlans();
    expect(await screen.findByText("Ticket T2 retried")).not.toBeNull();
    expect(screen.getByText("Plan cancelled: Wrong scope")).not.toBeNull();
  });
});

describe("staying current", () => {
  it("reads the plan again when a plan.updated event arrives", async () => {
    renderPlans();
    await screen.findByText("Add the route");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    detail = { ...detail, tickets: [ticket({ title: "Add the route, renamed" })] };
    api.send("/api/events", "plan.updated", { plan_id: "pl1", project_id: "p1" });
    expect(await screen.findByText("Add the route, renamed")).not.toBeNull();
  });

  it("ignores a merge_request.updated event, which nothing emits", async () => {
    renderPlans();
    await screen.findByText("Add the route");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    const before = api.made("GET", "/api/plans/pl1").length;
    api.send("/api/events", "merge_request.updated", { id: "mr1" });
    await new Promise((r) => setTimeout(r, 600));
    expect(api.made("GET", "/api/plans/pl1")).toHaveLength(before);
  });
});

describe("requesting changes", () => {
  it("offers the action only on tickets with a request in review", async () => {
    renderPlans();
    await screen.findByText("Add the test");
    expect(screen.getAllByRole("button", { name: "Request changes" })).toHaveLength(1);
  });

  it("sends the notes for the ticket and closes the dialog", async () => {
    api.on("POST", "/api/plans/pl1/tickets/T2/changes", () => ({ id: "run-new" }));
    renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Request changes" }));
    const dialog = within(await screen.findByRole("dialog"));
    const submit = dialog.getByRole("button", { name: "Request changes" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText(/Notes/), { target: { value: "Handle the empty query" } });
    fireEvent.click(submit);
    await waitFor(() => expect(api.made("POST", "/api/plans/pl1/tickets/T2/changes")).toHaveLength(1));
    expect(api.made("POST", "/api/plans/pl1/tickets/T2/changes")[0].body).toEqual({ notes: "Handle the empty query" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("shows the server's message when the request is refused", async () => {
    api.on("POST", "/api/plans/pl1/tickets/T2/changes", () => apiError(409, "run_live", "a run is working on the merge request"));
    renderPlans();
    fireEvent.click(await screen.findByRole("button", { name: "Request changes" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText(/Notes/), { target: { value: "x" } });
    fireEvent.click(dialog.getByRole("button", { name: "Request changes" }));
    expect((await dialog.findByRole("alert")).textContent).toBe("a run is working on the merge request");
  });

  it("reads the changes_requested event with its notes", async () => {
    detail.events = [event({ id: "e3", kind: "changes_requested", ticket_id: "T2", detail: { notes: "Handle the empty query" } })];
    renderPlans();
    expect(await screen.findByText("Changes requested on ticket T2")).not.toBeNull();
    expect(screen.getByText("Handle the empty query")).not.toBeNull();
  });
});
