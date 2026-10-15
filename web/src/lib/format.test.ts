import { describe, expect, it } from "vitest";
import { elapsedMs, formatCount, formatDuration, formatMicros, humanize, shortId } from "./format";

describe("formatMicros", () => {
  it("shows micro-dollars as dollars with four decimals", () => {
    expect(formatMicros(0)).toBe("$0.0000");
    expect(formatMicros(1)).toBe("$0.0000");
    expect(formatMicros(12_345)).toBe("$0.0123");
    expect(formatMicros(1_000_000)).toBe("$1.0000");
    expect(formatMicros(2_500_000)).toBe("$2.5000");
  });

  it("rounds half-way values up without float error", () => {
    expect(formatMicros(12_350)).toBe("$0.0124");
    expect(formatMicros(50)).toBe("$0.0001");
    expect(formatMicros(49)).toBe("$0.0000");
  });

  it("carries into the dollars and groups thousands", () => {
    expect(formatMicros(999_999)).toBe("$1.0000");
    expect(formatMicros(1_234_567_890)).toBe("$1,234.5679");
  });

  it("treats a missing value as zero and keeps the sign", () => {
    expect(formatMicros(undefined)).toBe("$0.0000");
    expect(formatMicros(null)).toBe("$0.0000");
    expect(formatMicros(-1_500_000)).toBe("-$1.5000");
  });
});

describe("formatDuration", () => {
  it("uses milliseconds below a second", () => {
    expect(formatDuration(0)).toBe("0ms");
    expect(formatDuration(850)).toBe("850ms");
  });

  it("uses one decimal below ten seconds and whole seconds below a minute", () => {
    expect(formatDuration(1_000)).toBe("1.0s");
    expect(formatDuration(1_250)).toBe("1.3s");
    expect(formatDuration(9_949)).toBe("9.9s");
    expect(formatDuration(42_000)).toBe("42s");
    expect(formatDuration(59_999)).toBe("59s");
  });

  it("uses minutes, hours and days", () => {
    expect(formatDuration(60_000)).toBe("1m 00s");
    expect(formatDuration(187_000)).toBe("3m 07s");
    expect(formatDuration(3_900_000)).toBe("1h 05m");
    expect(formatDuration(49 * 3_600_000)).toBe("2d 01h");
  });

  it("shows a dash for missing or invalid values", () => {
    expect(formatDuration(undefined)).toBe("—");
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(-5)).toBe("—");
    expect(formatDuration(Number.NaN)).toBe("—");
  });
});

describe("shortId", () => {
  it("keeps the random tail of a UUIDv7 so ids from the same minute differ", () => {
    const a = "0192f3a4-5b6c-7d8e-9f01-23456789abcd";
    const b = "0192f3a4-5b6d-7d8e-9f01-23456789ef01";
    expect(shortId(a)).toBe("6789abcd");
    expect(shortId(b)).toBe("6789ef01");
    expect(shortId(a)).not.toBe(shortId(b));
  });

  it("returns short ids whole and handles missing ids", () => {
    expect(shortId("abc123")).toBe("abc123");
    expect(shortId("")).toBe("—");
    expect(shortId(undefined)).toBe("—");
  });

  it("honours a length", () => {
    expect(shortId("0192f3a4-5b6c-7d8e-9f01-23456789abcd", 4)).toBe("abcd");
  });
});

describe("elapsedMs", () => {
  const start = "2026-10-15T10:00:00Z";
  it("measures between two timestamps", () => {
    expect(elapsedMs(start, "2026-10-15T10:00:03.500Z")).toBe(3500);
  });
  it("measures up to now when there is no end", () => {
    expect(elapsedMs(start, undefined, Date.parse("2026-10-15T10:01:00Z"))).toBe(60_000);
  });
  it("is undefined without a usable start and never negative", () => {
    expect(elapsedMs(undefined)).toBeUndefined();
    expect(elapsedMs("not a date")).toBeUndefined();
    expect(elapsedMs(start, "2026-10-15T09:59:00Z")).toBe(0);
  });
});

describe("small helpers", () => {
  it("formats counts and statuses", () => {
    expect(formatCount(1234567)).toBe("1,234,567");
    expect(formatCount(undefined)).toBe("0");
    expect(humanize("waiting_approval")).toBe("waiting approval");
  });
});
