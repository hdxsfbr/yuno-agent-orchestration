import { describe, expect, it } from "vitest";
import { describeCondition, formatList, formatValue, isCombined, parseList, parseValue } from "./condition";

describe("condition values", () => {
  it.each([
    ["approved", "approved"],
    ["3", 3],
    ["3.5", 3.5],
    ["true", true],
    ["null", null],
    ['"3"', "3"],
    ['"true"', "true"],
    ["", ""],
    ["a b", "a b"],
    ["3.", "3."],
  ])("reads %j as %j", (text, value) => {
    expect(parseValue(text)).toEqual(value);
  });

  it("writes a value so that reading it back gives the same value", () => {
    for (const v of ["approved", "3", "true", "", "a b", 3, true, null, 2.5]) {
      expect(parseValue(formatValue(v))).toEqual(v);
    }
  });

  it("reads and writes lists", () => {
    expect(parseList("approved, merged,3")).toEqual(["approved", "merged", 3]);
    expect(parseList("")).toEqual([]);
    expect(formatList(["approved", 3, "4"])).toBe('approved, 3, "4"');
    expect(parseList(formatList(["approved", 3, "4"]))).toEqual(["approved", 3, "4"]);
  });
});

describe("describeCondition", () => {
  it("summarises every shape on one line", () => {
    expect(describeCondition(undefined)).toBe("always");
    expect(describeCondition({ path: "/r/verdict", op: "eq", value: "approved" })).toBe("/r/verdict = approved");
    expect(describeCondition({ path: "/r/n", op: "gt", value: 3 })).toBe("/r/n > 3");
    expect(describeCondition({ path: "/r/v", op: "in", value: ["a", "b"] })).toBe("/r/v in [a, b]");
    expect(describeCondition({ path: "/r/v", op: "exists" })).toBe("/r/v exists");
    expect(describeCondition({ all: [{}, {}] })).toBe("all of 2 checks");
    expect(describeCondition({ any: [{}] })).toBe("any of 1 checks");
  });

  it("knows which conditions the form cannot show", () => {
    expect(isCombined({ all: [] })).toBe(true);
    expect(isCombined({ any: [{ path: "/a", op: "exists" }] })).toBe(true);
    expect(isCombined({ path: "/a", op: "eq", value: 1 })).toBe(false);
  });
});
