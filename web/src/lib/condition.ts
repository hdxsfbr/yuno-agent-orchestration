import type { Condition, ConditionOp } from "./api";

export const OP_LABEL: Record<ConditionOp, string> = {
  eq: "equals",
  neq: "does not equal",
  in: "is one of",
  exists: "exists",
  gt: "is greater than",
  lt: "is less than",
};

/** Conditions the single-check form cannot show: all/any, or anything with unknown shape. */
export function isCombined(c: Condition): boolean {
  return c.all !== undefined || c.any !== undefined;
}

/**
 * The text a value is edited as. Strings that would be read back as another type (the text
 * "3", "true") are quoted so the round trip keeps their type.
 */
export function formatValue(v: unknown): string {
  if (v === undefined) return "";
  if (typeof v === "string") {
    try {
      JSON.parse(v);
      return JSON.stringify(v); // "3" would otherwise come back as the number 3
    } catch {
      return v;
    }
  }
  return JSON.stringify(v);
}

/** Numbers, true, false, null, quoted text and JSON are read as such; anything else is text. */
export function parseValue(text: string): unknown {
  const t = text.trim();
  if (t === "") return "";
  try {
    return JSON.parse(t);
  } catch {
    return text;
  }
}

export function formatList(v: unknown): string {
  return Array.isArray(v) ? v.map(formatValue).join(", ") : formatValue(v);
}

/** Comma separated values, each read like a single value. */
export function parseList(text: string): unknown[] {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "")
    .map(parseValue);
}

/** One line for an edge label or an outline row. */
export function describeCondition(c: Condition | undefined): string {
  if (!c) return "always";
  if (c.all) return `all of ${c.all.length} checks`;
  if (c.any) return `any of ${c.any.length} checks`;
  const path = c.path || "(no path)";
  if (c.op === "exists") return `${path} exists`;
  const symbol: Partial<Record<ConditionOp, string>> = { eq: "=", neq: "≠", gt: ">", lt: "<", in: "in" };
  const value = c.op === "in" ? `[${formatList(c.value)}]` : formatValue(c.value);
  return `${path} ${symbol[c.op ?? "eq"] ?? c.op} ${value}`;
}
