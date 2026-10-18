import { describe, expect, it } from "vitest";
import { elapsedMs, formatAgo, formatBytes, formatCount, formatDuration, formatMicros, formatUsd, humanize, shortId } from "./format";

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

describe("formatUsd", () => {
  it("shows dollars with two decimals and a thousands separator", () => {
    expect(formatUsd(0)).toBe("$0.00");
    expect(formatUsd(5)).toBe("$5.00");
    expect(formatUsd(12.5)).toBe("$12.50");
    expect(formatUsd(1234.5)).toBe("$1,234.50");
    expect(formatUsd(1_000_000)).toBe("$1,000,000.00");
  });

  it("rounds to the cent and tolerates a missing amount", () => {
    expect(formatUsd(0.004)).toBe("$0.00");
    expect(formatUsd(0.126)).toBe("$0.13");
    expect(formatUsd(-2)).toBe("-$2.00");
    expect(formatUsd(undefined)).toBe("$0.00");
    expect(formatUsd(Number.NaN)).toBe("$0.00");
  });
});

describe("formatAgo", () => {
  const now = Date.parse("2026-10-17T12:00:00Z");
  const ago = (ms: number) => formatAgo(new Date(now - ms).toISOString(), now);

  it("reads seconds, minutes, hours and days", () => {
    expect(ago(0)).toBe("just now");
    expect(ago(9_000)).toBe("just now");
    expect(ago(10_000)).toBe("10s ago");
    expect(ago(59_000)).toBe("59s ago");
    expect(ago(60_000)).toBe("1 min ago");
    expect(ago(59 * 60_000)).toBe("59 min ago");
    expect(ago(60 * 60_000)).toBe("1 h ago");
    expect(ago(23 * 3_600_000)).toBe("23 h ago");
    expect(ago(24 * 3_600_000)).toBe("1 d ago");
    expect(ago(3 * 86_400_000 + 5_000)).toBe("3 d ago");
  });

  it("reads a time in the future as just now and a missing or unreadable one as a dash", () => {
    expect(formatAgo("2026-10-17T12:05:00Z", now)).toBe("just now");
    expect(formatAgo(undefined, now)).toBe("—");
    expect(formatAgo(null, now)).toBe("—");
    expect(formatAgo("soon", now)).toBe("—");
  });
});

describe("formatBytes", () => {
  it("counts small sizes in bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1)).toBe("1 B");
    expect(formatBytes(850)).toBe("850 B");
    expect(formatBytes(1023)).toBe("1023 B");
  });

  it("uses binary units, with one decimal below ten of a unit and none above", () => {
    expect(formatBytes(1024)).toBe("1 KiB");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(10 * 1024)).toBe("10 KiB");
    expect(formatBytes(262_144)).toBe("256 KiB");
    expect(formatBytes(300 * 1024)).toBe("300 KiB");
    expect(formatBytes(3.2 * 1024 * 1024)).toBe("3.2 MiB");
    expect(formatBytes(20 * 1024 * 1024)).toBe("20 MiB");
    expect(formatBytes(50 * 1024 ** 3)).toBe("50 GiB");
  });

  it("moves to the next unit when rounding reaches 1024", () => {
    expect(formatBytes(1024 * 1024 - 1)).toBe("1 MiB");
    expect(formatBytes(1024 * 1024)).toBe("1 MiB");
    expect(formatBytes(9.97 * 1024)).toBe("10 KiB");
  });

  it("shows an em dash for a size it cannot read", () => {
    expect(formatBytes(undefined)).toBe("—");
    expect(formatBytes(null)).toBe("—");
    expect(formatBytes(-1)).toBe("—");
    expect(formatBytes(Number.NaN)).toBe("—");
  });
});
