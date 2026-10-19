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
    expect(screen.getByText("Building")).not.toBeNull();
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
    expect(screen.getByText("Done")).not.toBeNull();
  });

  it("says so when no plan is open", async () => {
    plans = [];
    renderPlans();
    expect(await screen.findByText(/No plan is open/)).not.toBeNull();
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
