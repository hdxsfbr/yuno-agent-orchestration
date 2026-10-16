// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { Schedule, ScheduleInput, Workflow } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import SchedulesPage from "./SchedulesPage";

const RUN = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";
const BROWSER_ZONE = "America/Sao_Paulo";

const workflow = (over: Partial<Workflow>): Workflow =>
  ({ id: "w1", name: "Nightly report", description: "", version: 1, created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T09:00:00Z", definition: { nodes: [], edges: [] }, ...over }) as Workflow;

const reportSchema = {
  type: "object",
  properties: { topic: { type: "string", description: "What to report on" }, days: { type: "integer" } },
  required: ["topic"],
};

const schedule = (over: Partial<Schedule>): Schedule => ({
  id: "s1",
  workflow_id: "w1",
  kind: "cron",
  cron_expr: "0 9 * * 1-5",
  tz: "America/Sao_Paulo",
  enabled: true,
  input: { topic: "sales", days: 7 },
  next_run_at: "2026-10-16T12:00:00Z",
  last_run_at: "2026-10-15T12:00:00Z",
  last_run_id: RUN,
  created_at: "2026-10-14T09:00:00Z",
  updated_at: "2026-10-14T09:00:00Z",
  ...over,
});

let workflows: Workflow[];
let schedules: Schedule[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  const original = Intl.DateTimeFormat.prototype.resolvedOptions;
  vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions").mockImplementation(function (this: Intl.DateTimeFormat) {
    return { ...original.call(this), timeZone: BROWSER_ZONE };
  });
  workflows = [
    workflow({ definition: { nodes: [], edges: [], input_schema: reportSchema } }),
    workflow({ id: "w2", name: "Release train" }),
    workflow({ id: "w3", name: "Dev pipeline", template_key: "dev_pipeline", definition: { nodes: [{ id: "c", kind: "agent", agent_name: "Coder" }], edges: [] } }),
    workflow({ id: "w4", name: "Weekly digest", definition: { nodes: [], edges: [], input_schema: { type: "object", properties: { topic: { type: "string" } } } } }),
  ];
  schedules = [
    schedule({}),
    schedule({
      id: "s2",
      workflow_id: "w2",
      kind: "delay",
      cron_expr: undefined,
      run_at: "2026-10-20T15:00:00Z",
      tz: "UTC",
      enabled: false,
      input: {},
      next_run_at: null,
      last_run_at: null,
      last_run_id: null,
    }),
    schedule({ id: "s3", workflow_id: "gone", cron_expr: "*/5 * * * *", tz: "UTC", last_error: "workflow deleted", input: {} }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/workflows", () => workflows);
  api.on("GET", "/api/schedules", () => schedules);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/schedules"]}>
        <Routes>
          <Route path="/schedules" element={<SchedulesPage />} />
          <Route path="/runs/:id" element={<p>run page</p>} />
          <Route path="/workflows" element={<p>workflows page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const row = async (name: RegExp | string) => within((await screen.findByRole("switch", { name })).closest("tr") as HTMLElement);
const dialog = async () => within(await screen.findByRole("dialog"));
const openNew = async () => {
  fireEvent.click(await screen.findByRole("button", { name: "New schedule" }));
  return dialog();
};

describe("SchedulesPage list", () => {
  it("shows workflow, kind, expression, zone, next run, last run and the last error", async () => {
    renderPage();
    const cron = await row(/Enable Nightly report \(0 9/);
    expect(cron.getByRole("link", { name: "Nightly report" }).getAttribute("href")).toBe("/workflows/w1");
    expect(cron.getByText("Repeats")).not.toBeNull();
    expect(cron.getByText("0 9 * * 1-5")).not.toBeNull();
    expect(cron.getByText("America/Sao_Paulo")).not.toBeNull();
    expect(cron.getByText(formatDateTime("2026-10-16T12:00:00Z"))).not.toBeNull();
    expect(cron.getByText(formatDateTime("2026-10-15T12:00:00Z"))).not.toBeNull();
    expect(cron.getByRole("link", { name: `Run ${shortId(RUN)}` }).getAttribute("href")).toBe(`/runs/${RUN}`);
    expect((cron.getByRole("switch") as HTMLInputElement).checked).toBe(true);

    const once = await row(/Enable Release train/);
    expect(once.getByText("Once")).not.toBeNull();
    expect(once.getByText(`At ${formatDateTime("2026-10-20T15:00:00Z")}`)).not.toBeNull();
    expect(once.getByText("Never")).not.toBeNull();
    expect((once.getByRole("switch") as HTMLInputElement).checked).toBe(false);

    const broken = await row(/Enable Deleted workflow/);
    expect(broken.getByText("workflow deleted")).not.toBeNull();
  });

  it("says so when there are none", async () => {
    schedules = [];
    renderPage();
    expect(await screen.findByText(/No schedules yet/)).not.toBeNull();
  });

  it("keeps the name of a deleted workflow, marked as deleted and no longer a link", async () => {
    schedules = [schedule({ id: "s4", workflow_id: "gone", workflow_name: "Old digest", enabled: false, next_run_at: null })];
    renderPage();
    const r = await row(/Enable Old digest/);
    expect(r.getByText("Old digest")).not.toBeNull();
    expect(r.getByText("Deleted")).not.toBeNull();
    expect(r.queryByRole("link", { name: "Old digest" })).toBeNull();
  });

  it("shows a renamed workflow under its new name", async () => {
    schedules = [schedule({ workflow_name: "Nightly report (old name)" })];
    renderPage();
    const r = await row(/Enable Nightly report \(0 9/);
    expect(r.getByRole("link", { name: "Nightly report" })).not.toBeNull();
  });
});

describe("enabled toggle", () => {
  it("sends the schedule unchanged except for enabled and shows the answer", async () => {
    api.on("PUT", /^\/api\/schedules\/([^/]+)$/, (r) => {
      const next = { ...schedules.find((s) => s.id === r.match[1])!, ...(r.body as ScheduleInput) };
      schedules = schedules.map((s) => (s.id === next.id ? next : s));
      return next;
    });
    renderPage();
    const once = await row(/Enable Release train/);
    fireEvent.click(once.getByRole("switch"));
    await waitFor(() => expect(api.made("PUT", "/api/schedules/s2")).toHaveLength(1));
    expect(api.made("PUT", "/api/schedules/s2")[0].body).toEqual({
      workflow_id: "w2",
      kind: "delay",
      run_at: "2026-10-20T15:00:00Z",
      tz: "UTC",
      enabled: true,
      input: {},
    });
    await waitFor(() => expect((once.getByRole("switch") as HTMLInputElement).checked).toBe(true));
  });

  it("reports a refusal and leaves the switch where it was", async () => {
    api.on("PUT", /^\/api\/schedules\/[^/]+$/, () => apiError(422, "validation_failed", "run_at is in the past"));
    renderPage();
    const once = await row(/Enable Release train/);
    fireEvent.click(once.getByRole("switch"));
    expect((await screen.findByText(/Could not change the schedule: run_at is in the past/)).getAttribute("role")).toBe("alert");
    expect((once.getByRole("switch") as HTMLInputElement).checked).toBe(false);
  });
});

describe("creating a schedule", () => {
  it("builds a repeating schedule with the input from the workflow's schema, in the browser's zone by default", async () => {
    api.on("POST", "/api/schedules", (r) => ({ ...schedule({ id: "s9" }), ...(r.body as ScheduleInput) }));
    renderPage();
    const d = await openNew();
    await waitFor(() => expect(d.getByRole("option", { name: "Nightly report" })).not.toBeNull());
    expect(d.queryByRole("option", { name: "Dev pipeline" })).toBeNull();
    expect((d.getByLabelText("Runs") as HTMLSelectElement).value).toBe("cron");

    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w1" } });
    expect((d.getByLabelText("Time zone") as HTMLSelectElement).value).toBe(BROWSER_ZONE);
    fireEvent.change(d.getByLabelText(/^Cron expression/), { target: { value: " 30 8 * * * " } });
    fireEvent.change(d.getByLabelText("Time zone"), { target: { value: "Europe/London" } });
    fireEvent.change(d.getByLabelText(/^topic/), { target: { value: "revenue" } });
    fireEvent.change(d.getByLabelText("days"), { target: { value: "14" } });
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));

    await waitFor(() => expect(api.made("POST", "/api/schedules")).toHaveLength(1));
    expect(api.made("POST", "/api/schedules")[0].body).toEqual({
      workflow_id: "w1",
      kind: "cron",
      cron_expr: "30 8 * * *",
      tz: "Europe/London",
      enabled: true,
      input: { topic: "revenue", days: 14 },
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("builds a one-off schedule from a date and time", async () => {
    api.on("POST", "/api/schedules", (r) => ({ ...schedule({ id: "s9" }), ...(r.body as ScheduleInput) }));
    renderPage();
    const d = await openNew();
    await waitFor(() => expect(d.getByRole("option", { name: "Release train" })).not.toBeNull());
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w2" } });
    fireEvent.change(d.getByLabelText("Runs"), { target: { value: "delay" } });
    expect(d.queryByLabelText(/^Cron expression/)).toBeNull();
    expect(d.getByText(`In your time zone (${BROWSER_ZONE}).`)).not.toBeNull();
    fireEvent.change(d.getByLabelText(/^Run at/), { target: { value: "2026-10-21T09:30" } });
    fireEvent.click(d.getByLabelText(/^Enabled/));
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));

    await waitFor(() => expect(api.made("POST", "/api/schedules")).toHaveLength(1));
    expect(api.made("POST", "/api/schedules")[0].body).toEqual({
      workflow_id: "w2",
      kind: "delay",
      run_at: new Date("2026-10-21T09:30").toISOString(),
      tz: BROWSER_ZONE,
      enabled: false,
      input: {},
    });
  });

  it("takes the whole input as JSON when the workflow declares no fields", async () => {
    api.on("POST", "/api/schedules", (r) => ({ ...schedule({ id: "s9" }), ...(r.body as ScheduleInput) }));
    renderPage();
    const d = await openNew();
    await waitFor(() => expect(d.getByRole("option", { name: "Release train" })).not.toBeNull());
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w2" } });
    fireEvent.change(d.getByLabelText(/^Cron expression/), { target: { value: "0 * * * *" } });
    fireEvent.change(d.getByLabelText("Input"), { target: { value: "{broken" } });
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));
    expect((await d.findByText(/Not valid JSON/)).closest("[role=alert]")).not.toBeNull();
    expect(api.made("POST", "/api/schedules")).toHaveLength(0);

    fireEvent.change(d.getByLabelText("Input"), { target: { value: '{"channel":"ops"}' } });
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(api.made("POST", "/api/schedules")).toHaveLength(1));
    expect((api.made("POST", "/api/schedules")[0].body as ScheduleInput).input).toEqual({ channel: "ops" });
  });

  it("asks for what is missing before sending anything", async () => {
    renderPage();
    const d = await openNew();
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));
    expect(await d.findByText("A workflow is required")).not.toBeNull();
    expect(d.getByText("Enter a cron expression")).not.toBeNull();
    fireEvent.change(d.getByLabelText("Runs"), { target: { value: "delay" } });
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));
    expect(await d.findByText("Choose the date and time to run")).not.toBeNull();
    expect(api.made("POST", "/api/schedules")).toHaveLength(0);
  });

  it("puts the API's 422 next to the cron expression, the zone and the input field it names", async () => {
    api.on("POST", "/api/schedules", () =>
      apiError(422, "validation_failed", "validation failed", [
        { path: "cron_expr", message: "expected exactly 5 fields, found 2: [foo bar]" },
        { path: "tz", message: "unknown time zone Mars/Base" },
        { path: "input.topic", message: "topic is required" },
      ]),
    );
    renderPage();
    const d = await openNew();
    await waitFor(() => expect(d.getByRole("option", { name: "Nightly report" })).not.toBeNull());
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w1" } });
    fireEvent.change(d.getByLabelText(/^Cron expression/), { target: { value: "foo bar" } });
    fireEvent.change(d.getByLabelText(/^topic/), { target: { value: "x" } });
    fireEvent.click(d.getByRole("button", { name: "Create schedule" }));

    await waitFor(() => expect(d.getByLabelText(/^Cron expression/).parentElement?.textContent).toContain("expected exactly 5 fields"));
    expect(d.getByLabelText(/^Cron expression/).getAttribute("aria-invalid")).toBe("true");
    expect(d.getByLabelText("Time zone").parentElement?.textContent).toContain("unknown time zone Mars/Base");
    expect(d.getByLabelText(/^topic/).parentElement?.textContent).toContain("topic is required");
    expect(d.getByLabelText("days").getAttribute("aria-invalid")).toBeNull();
    expect(screen.getByRole("dialog")).not.toBeNull();
  });

  it("offers a way to make a workflow when none can be scheduled", async () => {
    workflows = [workflows[2]];
    renderPage();
    const d = await openNew();
    expect(await d.findByRole("link", { name: "Create a workflow" })).not.toBeNull();
    expect((d.getByRole("button", { name: "Create schedule" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("editing a schedule", () => {
  it("opens on the saved values, field by field, and saves to the same schedule", async () => {
    api.on("PUT", /^\/api\/schedules\/s1$/, (r) => ({ ...schedules[0], ...(r.body as ScheduleInput) }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Edit Nightly report/ }));
    const d = await dialog();
    await waitFor(() => expect((d.getByLabelText(/^Workflow/) as HTMLSelectElement).value).toBe("w1"));
    expect((d.getByLabelText(/^Cron expression/) as HTMLInputElement).value).toBe("0 9 * * 1-5");
    expect((d.getByLabelText("Time zone") as HTMLSelectElement).value).toBe("America/Sao_Paulo");
    expect((d.getByLabelText(/^topic/) as HTMLInputElement).value).toBe("sales");
    expect((d.getByLabelText("days") as HTMLInputElement).value).toBe("7");

    fireEvent.change(d.getByLabelText(/^topic/), { target: { value: "returns" } });
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.made("PUT", "/api/schedules/s1")).toHaveLength(1));
    expect(api.made("PUT", "/api/schedules/s1")[0].body).toEqual({
      workflow_id: "w1",
      kind: "cron",
      cron_expr: "0 9 * * 1-5",
      tz: "America/Sao_Paulo",
      enabled: true,
      input: { topic: "returns", days: 7 },
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("opens a one-off schedule on its time, in the viewer's zone", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Edit Release train/ }));
    const d = await dialog();
    await waitFor(() => expect((d.getByLabelText("Runs") as HTMLSelectElement).value).toBe("delay"));
    const local = new Date("2026-10-20T15:00:00Z");
    const pad = (n: number) => String(n).padStart(2, "0");
    expect((d.getByLabelText(/^Run at/) as HTMLInputElement).value).toBe(`${local.getFullYear()}-${pad(local.getMonth() + 1)}-${pad(local.getDate())}T${pad(local.getHours())}:${pad(local.getMinutes())}`);
  });

  it("starts the input afresh when another workflow is chosen", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Edit Nightly report/ }));
    const d = await dialog();
    await waitFor(() => expect((d.getByLabelText(/^topic/) as HTMLInputElement).value).toBe("sales"));
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w2" } });
    expect(d.queryByLabelText(/^topic/)).toBeNull();
    expect((d.getByLabelText("Input") as HTMLTextAreaElement).value).toBe("{}");
  });

  it("does not carry typed values over to the next workflow, even one with a field of the same name", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Edit Nightly report/ }));
    const d = await dialog();
    await waitFor(() => expect((d.getByLabelText(/^topic/) as HTMLInputElement).value).toBe("sales"));
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w4" } });
    expect((d.getByLabelText("topic") as HTMLInputElement).value).toBe("");

    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w2" } });
    fireEvent.change(d.getByLabelText("Input"), { target: { value: '{"a":1}' } });
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w4" } });
    fireEvent.change(d.getByLabelText(/^Workflow/), { target: { value: "w2" } });
    expect((d.getByLabelText("Input") as HTMLTextAreaElement).value).toBe("{}");
  });
});

describe("deleting a schedule", () => {
  it("asks first, and removes it only when confirmed", async () => {
    api.on("DELETE", /^\/api\/schedules\/([^/]+)$/, (r) => {
      schedules = schedules.filter((s) => s.id !== r.match[1]);
      return new Response(null, { status: 204 });
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Delete Nightly report/ }));
    let d = await dialog();
    expect(d.getByText(/Delete the Nightly report schedule\?/)).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.made("DELETE", /schedules/)).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: /Delete Nightly report/ }));
    d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete schedule" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /Delete Nightly report/ })).toBeNull());
    expect(api.made("DELETE", "/api/schedules/s1")).toHaveLength(1);
    expect(screen.getByRole("button", { name: /Delete Release train/ })).not.toBeNull();
  });

  it("keeps the dialog open with the reason when the delete fails", async () => {
    api.on("DELETE", /^\/api\/schedules\/[^/]+$/, () => apiError(500, "internal", "database is busy"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Delete Nightly report/ }));
    const d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete schedule" }));
    expect((await d.findByRole("alert")).textContent).toContain("database is busy");
  });
});

describe("live updates", () => {
  it("shows a fired schedule's new last and next run at once", async () => {
    renderPage();
    const cron = await row(/Enable Nightly report/);
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    const NEW_RUN = "01a14161-d5ef-7d96-ba96-ffffffffffff";
    schedules = [schedule({ last_run_at: "2026-10-16T12:00:00Z", last_run_id: NEW_RUN, next_run_at: "2026-10-17T12:00:00Z" }), ...schedules.slice(1)];
    api.send("/api/events", "schedule.fired", { schedule: schedules[0], run: { id: NEW_RUN } });
    await waitFor(() => expect(cron.getByRole("link", { name: `Run ${shortId(NEW_RUN)}` })).not.toBeNull());
    expect(cron.getByText(formatDateTime("2026-10-17T12:00:00Z"))).not.toBeNull();
  });

  it("shows a one-off schedule as switched off once it has fired", async () => {
    renderPage();
    const once = await row(/Enable Release train/);
    schedules[0] = schedule({});
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    const fired = schedule({ id: "s2", workflow_id: "w2", kind: "delay", run_at: "2026-10-20T15:00:00Z", enabled: false, next_run_at: null, last_run_at: "2026-10-20T15:00:01Z", last_run_id: RUN, input: {} });
    schedules[1] = fired;
    api.send("/api/events", "schedule.fired", { schedule: fired, run: { id: RUN } });
    await waitFor(() => expect(once.getByRole("link", { name: `Run ${shortId(RUN)}` })).not.toBeNull());
    expect((once.getByRole("switch") as HTMLInputElement).checked).toBe(false);
  });
});
