import { describe, expect, it, vi } from "vitest";
import type { Schedule } from "./api";
import { browserTimeZone, buildSchedule, describeWhen, firstDetail, fromLocalInput, scheduleErrors, scheduleToInput, timeZones, toLocalInput, type ScheduleFormState } from "./schedule";
import { inputFields } from "./runInput";

const form = (over: Partial<ScheduleFormState> = {}): ScheduleFormState => ({
  workflowId: "w1",
  projectId: "",
  mergeRequestId: "",
  kind: "cron",
  cronExpr: "0 9 * * *",
  tz: "Europe/London",
  runAt: "",
  enabled: true,
  draft: {},
  whole: "{}",
  ...over,
});

const fields = inputFields({ type: "object", properties: { topic: { type: "string" }, days: { type: "integer" } }, required: ["topic"] });

describe("timeZones", () => {
  it("always offers UTC first and the zones it is asked for, once each", () => {
    const zones = timeZones("Mars/Base", "UTC", "");
    expect(zones[0]).toBe("UTC");
    expect(zones.filter((z) => z === "UTC")).toHaveLength(1);
    expect(zones).toContain("Mars/Base");
    expect(zones).toContain("America/Sao_Paulo");
    expect(zones).not.toContain("");
  });
});

describe("browserTimeZone", () => {
  it("reads the viewer's zone and falls back to UTC", () => {
    const spy = vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions");
    spy.mockReturnValueOnce({ timeZone: "Asia/Tokyo" } as Intl.ResolvedDateTimeFormatOptions);
    expect(browserTimeZone()).toBe("Asia/Tokyo");
    spy.mockReturnValueOnce({ timeZone: "" } as Intl.ResolvedDateTimeFormatOptions);
    expect(browserTimeZone()).toBe("UTC");
    spy.mockRestore();
  });
});

describe("local date and time", () => {
  it("round-trips an instant through a datetime-local value in the viewer's zone", () => {
    const iso = "2026-10-21T12:30:00.000Z";
    expect(fromLocalInput(toLocalInput(iso))).toBe(iso);
  });

  it("refuses anything that is not a date and time", () => {
    expect(fromLocalInput("")).toBeUndefined();
    expect(fromLocalInput("tomorrow")).toBeUndefined();
    expect(fromLocalInput("2026-13-45T99:99")).toBeUndefined();
    expect(toLocalInput(null)).toBe("");
    expect(toLocalInput("not a date")).toBe("");
  });
});

describe("buildSchedule", () => {
  it("builds a repeating schedule, trimming the expression and reading the input fields", () => {
    const built = buildSchedule(form({ cronExpr: " 30 8 * * * ", draft: { topic: "sales", days: "7" } }), fields);
    expect(built.errors).toEqual({});
    expect(built.value).toEqual({ workflow_id: "w1", kind: "cron", cron_expr: "30 8 * * *", tz: "Europe/London", enabled: true, input: { topic: "sales", days: 7 } });
  });

  it("builds a one-off schedule with an absolute time and no expression", () => {
    const built = buildSchedule(form({ kind: "delay", cronExpr: "ignored", runAt: "2026-10-21T09:30", enabled: false }), []);
    expect(built.value).toEqual({ workflow_id: "w1", kind: "delay", run_at: new Date("2026-10-21T09:30").toISOString(), tz: "Europe/London", enabled: false, input: {} });
    expect(built.value).not.toHaveProperty("cron_expr");
  });

  it("names every missing piece by the path the API would use", () => {
    const built = buildSchedule(form({ workflowId: "", cronExpr: " ", tz: "", draft: { topic: "" } }), fields);
    expect(built.value).toBeUndefined();
    expect(Object.keys(built.errors).sort()).toEqual(["cron_expr", "input.topic", "tz", "workflow_id"]);
    expect(buildSchedule(form({ kind: "delay", runAt: "" }), []).errors).toHaveProperty("run_at");
  });

  it("names the project only for a workflow that works on one, and asks for it when none is chosen", () => {
    expect(buildSchedule(form({ projectId: "p1" }), []).value).not.toHaveProperty("project_id");
    expect(buildSchedule(form({ projectId: "p1" }), [], true).value?.project_id).toBe("p1");
    const missing = buildSchedule(form({ projectId: "" }), [], true);
    expect(missing.value).toBeUndefined();
    expect(missing.errors).toEqual({ project_id: ["Choose a project"] });
  });

  describe("the merge request", () => {
    const on = (over: Partial<ScheduleFormState> = {}) => form({ projectId: "p1", ...over });

    it("is sent when one is chosen for a new schedule, and left out when none is", () => {
      expect(buildSchedule(on({ mergeRequestId: "mr1" }), [], true).value?.merge_request_id).toBe("mr1");
      expect(buildSchedule(on(), [], true).value).not.toHaveProperty("merge_request_id");
    });

    it("is left out on an edit that leaves the choice as it was saved, so the stored one stays", () => {
      expect(buildSchedule(on({ mergeRequestId: "mr1" }), [], true, "mr1").value).not.toHaveProperty("merge_request_id");
      expect(buildSchedule(on(), [], true, "").value).not.toHaveProperty("merge_request_id");
    });

    it("is sent as an empty string when the saved one was cleared, and as the new id when another was chosen", () => {
      expect(buildSchedule(on({ mergeRequestId: "" }), [], true, "mr1").value?.merge_request_id).toBe("");
      expect(buildSchedule(on({ mergeRequestId: "mr2" }), [], true, "mr1").value?.merge_request_id).toBe("mr2");
    });

    it("is never sent for a workflow that works on no project, except to clear one a schedule still names", () => {
      expect(buildSchedule(form({ mergeRequestId: "mr1" }), []).value).not.toHaveProperty("merge_request_id");
      expect(buildSchedule(form({ mergeRequestId: "mr1" }), [], false, "mr1").value?.merge_request_id).toBe("");
    });
  });

  it("reads the whole input as a JSON object when the workflow has no fields", () => {
    expect(buildSchedule(form({ whole: '{"a":1}' }), []).value?.input).toEqual({ a: 1 });
    expect(buildSchedule(form({ whole: "[1]" }), []).errors.input).toEqual(["The input must be a JSON object"]);
    expect(buildSchedule(form({ whole: "{" }), []).errors.input?.[0]).toMatch(/^Not valid JSON/);
  });
});

describe("scheduleErrors", () => {
  it("keys a list of {path, message} by path, keeping input.<field> whole", () => {
    expect(
      scheduleErrors([
        { path: "cron_expr", message: "bad" },
        { path: "input.topic", message: "required" },
        { path: "input.topic", message: "too short" },
      ]),
    ).toEqual({ cron_expr: ["bad"], "input.topic": ["required", "too short"] });
  });

  it("falls back to the agent form's reader for other shapes", () => {
    expect(scheduleErrors({ tz: "unknown zone" })).toEqual({ tz: ["unknown zone"] });
    expect(scheduleErrors(undefined)).toEqual({});
  });
});

describe("firstDetail", () => {
  it("is the message of the first entry of a list of {path, message}", () => {
    expect(firstDetail([{ path: "merge_request_id", message: "merge request !4 is merged" }, { path: "tz", message: "bad" }])).toBe("merge request !4 is merged");
  });

  it("skips entries without a message and is undefined when none has one", () => {
    expect(firstDetail([{ path: "a" }, null, { path: "b", message: " " }, { path: "c", message: "third" }])).toBe("third");
    expect(firstDetail([{ path: "a" }])).toBeUndefined();
    expect(firstDetail([])).toBeUndefined();
    expect(firstDetail(undefined)).toBeUndefined();
    expect(firstDetail({ tz: "unknown zone" })).toBeUndefined();
  });
});

describe("schedule display and round trip", () => {
  const base: Schedule = {
    id: "s1",
    workflow_id: "w1",
    kind: "cron",
    cron_expr: "0 9 * * *",
    tz: "UTC",
    enabled: true,
    input: { topic: "x" },
    created_at: "2026-10-14T09:00:00Z",
    updated_at: "2026-10-14T09:00:00Z",
  };

  it("describes a cron schedule by its expression and a one-off by its time", () => {
    expect(describeWhen(base)).toBe("0 9 * * *");
    expect(describeWhen({ kind: "delay", run_at: "2026-10-20T15:00:00Z" })).toMatch(/^At Oct 20,/);
  });

  it("leaves the merge request out of a request made from a stored schedule, so a toggle keeps it", () => {
    const named: Schedule = { ...base, project_id: "p1", merge_request_id: "mr1", merge_request_number: 12 };
    expect(scheduleToInput(named)).not.toHaveProperty("merge_request_id");
    expect(scheduleToInput(named)).not.toHaveProperty("merge_request_number");
  });

  it("turns a schedule back into a request without the fields the server owns", () => {
    expect(scheduleToInput(base)).toEqual({ workflow_id: "w1", kind: "cron", cron_expr: "0 9 * * *", tz: "UTC", enabled: true, input: { topic: "x" } });
    expect(scheduleToInput({ ...base, project_id: "p1", project_name: "yuno-playground" })).toEqual({
      workflow_id: "w1",
      project_id: "p1",
      kind: "cron",
      cron_expr: "0 9 * * *",
      tz: "UTC",
      enabled: true,
      input: { topic: "x" },
    });
    expect(scheduleToInput({ ...base, kind: "delay", cron_expr: undefined, run_at: "2026-10-20T15:00:00Z", input: null as unknown as Record<string, unknown> })).toEqual({
      workflow_id: "w1",
      kind: "delay",
      run_at: "2026-10-20T15:00:00Z",
      tz: "UTC",
      enabled: true,
      input: {},
    });
  });
});
