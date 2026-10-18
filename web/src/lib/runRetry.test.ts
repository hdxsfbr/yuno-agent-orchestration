import { describe, expect, it } from "vitest";
import { canRetry, exhaustedLimits, repeatsSideEffects, retryLimits, startingDraft, validateRetry, type RetryDraft } from "./runRetry";

describe("canRetry", () => {
  it("needs an unsuccessful end and a step to retry from", () => {
    for (const status of ["failed", "timed_out", "cancelled"] as const) {
      expect(canRetry({ status, current_node: "coder" })).toBe(true);
      expect(canRetry({ status })).toBe(false);
      expect(canRetry({ status, current_node: "" })).toBe(false);
    }
    for (const status of ["queued", "running", "waiting_approval", "succeeded"] as const) {
      expect(canRetry({ status, current_node: "coder" })).toBe(false);
    }
  });
});

describe("exhaustedLimits", () => {
  it("reads the {path, message} details of the 409", () => {
    expect(
      exhaustedLimits([
        { path: "max_steps", message: "40 of 40 used" },
        { path: "max_cost_usd", message: "$2.10 of $2.00 used" },
      ]),
    ).toEqual({ max_steps: ["40 of 40 used"], max_cost_usd: ["$2.10 of $2.00 used"] });
  });

  it("drops the limit's own name from a sentence that starts with it, as the API words it", () => {
    expect(
      exhaustedLimits([
        { path: "max_steps", message: "max_steps: 40 of 40 used" },
        { path: "max_cost_usd", message: "max_cost_usd: $2.10 of $2.00 used" },
      ]),
    ).toEqual({ max_steps: ["40 of 40 used"], max_cost_usd: ["$2.10 of $2.00 used"] });
  });

  it("reads `<limit>: <sentence>` strings and a prefixed path", () => {
    expect(exhaustedLimits(["max_tool_calls: 60 of 60 used"])).toEqual({ max_tool_calls: ["60 of 60 used"] });
    expect(exhaustedLimits([{ path: "limits.max_steps", message: "40 of 40 used" }])).toEqual({ max_steps: ["40 of 40 used"] });
  });

  it("is empty for details it cannot read", () => {
    expect(exhaustedLimits(undefined)).toEqual({});
    expect(exhaustedLimits(null)).toEqual({});
    expect(exhaustedLimits([])).toEqual({});
  });
});

describe("startingDraft", () => {
  it("starts from the run's own limits", () => {
    expect(startingDraft({ max_steps: 40, max_tool_calls: 60, max_cost_usd: 2.5, max_loop_visits: 3 }, {})).toEqual({
      max_steps: "40",
      max_tool_calls: "60",
      max_cost_usd: "2.5",
    });
  });

  it("falls back to the cap the server's sentence names, and to empty", () => {
    expect(startingDraft({}, { max_steps: ["40 of 40 used"], max_cost_usd: ["$2.10 of $2.00 used"], max_tool_calls: ["unreadable"] })).toEqual({
      max_steps: "40",
      max_tool_calls: "",
      max_cost_usd: "2.00",
    });
    expect(startingDraft({ max_steps: 50 }, { max_steps: ["40 of 40 used"] }).max_steps).toBe("50");
  });
});

describe("validateRetry", () => {
  const start: RetryDraft = { max_steps: "40", max_tool_calls: "60", max_cost_usd: "2" };
  const used = new Set(["max_steps"]);

  it("passes when the exhausted limit is raised and the others are left", () => {
    expect(validateRetry({ ...start, max_steps: "80" }, start, used)).toEqual({});
  });

  it("asks an exhausted limit to rise above where it started, and to be filled", () => {
    expect(validateRetry(start, start, used).max_steps?.[0]).toContain("raise it above 40");
    expect(validateRetry({ ...start, max_steps: "39" }, start, used).max_steps).toBeDefined();
    expect(validateRetry({ ...start, max_steps: "" }, start, used).max_steps?.[0]).toContain("enter a higher limit");
  });

  it("does not ask a limit that is not used up to rise, or to be filled", () => {
    expect(validateRetry({ ...start, max_steps: "80", max_tool_calls: "60", max_cost_usd: "" }, start, used)).toEqual({});
  });

  it("refuses text that is not a positive number, and a fraction of a step", () => {
    const e = validateRetry({ max_steps: "4.5", max_tool_calls: "abc", max_cost_usd: "0" }, start, new Set());
    expect(e.max_steps?.[0]).toBe("Max steps must be a whole number");
    expect(e.max_tool_calls?.[0]).toBe("Max tool calls must be a whole number");
    expect(e.max_cost_usd?.[0]).toBe("Max cost (USD) must be greater than 0");
    expect(validateRetry({ ...start, max_cost_usd: "x" }, start, new Set()).max_cost_usd?.[0]).toBe("Max cost (USD) must be a number");
  });

  it("has nothing to compare an exhausted limit with when it started empty", () => {
    expect(validateRetry({ max_steps: "1", max_tool_calls: "", max_cost_usd: "" }, { max_steps: "", max_tool_calls: "", max_cost_usd: "" }, used)).toEqual({});
  });
});

describe("retryLimits", () => {
  it("keeps every limit the run had and takes the three counted ones from the fields", () => {
    expect(
      retryLimits(
        { max_steps: 40, max_duration_seconds: 900, max_loop_visits: 3, max_cost_usd: 2 },
        { max_steps: "80", max_tool_calls: "70", max_cost_usd: "2" },
      ),
    ).toEqual({ max_steps: 80, max_duration_seconds: 900, max_loop_visits: 3, max_cost_usd: 2, max_tool_calls: 70 });
  });

  it("drops a field that was emptied, which falls back to the platform default", () => {
    expect(retryLimits({ max_steps: 40, max_cost_usd: 2 }, { max_steps: "", max_tool_calls: "", max_cost_usd: "3.5" })).toEqual({ max_cost_usd: 3.5 });
  });
});

describe("repeatsSideEffects", () => {
  const call = (tool_name: string, status = "executed", is_error = false) => ({ tool_name, status: status as "executed", is_error });
  it("is true for an executed deploy or GitLab API request, with or without the platform prefix", () => {
    expect(repeatsSideEffects([call("deploy_release")])).toBe(true);
    expect(repeatsSideEffects([call("yuno__gitlab_create_mr")])).toBe(true);
  });
  it("is false for anything else, for a call that did not happen and for an error result", () => {
    expect(repeatsSideEffects([])).toBe(false);
    expect(repeatsSideEffects([call("open_merge_request"), call("read")])).toBe(false);
    expect(repeatsSideEffects([call("deploy_release", "awaiting_approval"), call("deploy_release", "executed", true)])).toBe(false);
  });
});
