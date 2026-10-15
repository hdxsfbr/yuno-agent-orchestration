// Display helpers shared by the pages. Pure functions so they can be unit tested.

/**
 * Micro-dollars as dollars with four decimals. Integer arithmetic keeps half-way values
 * exact (12 350 micros is $0.0124, not whatever a float prints).
 */
export function formatMicros(micros: number | null | undefined): string {
  const m = Number.isFinite(micros) ? (micros as number) : 0;
  const sign = m < 0 ? "-" : "";
  const tenThousandths = Math.round(Math.abs(m) / 100);
  const whole = Math.floor(tenThousandths / 10_000);
  const frac = String(tenThousandths % 10_000).padStart(4, "0");
  return `${sign}$${whole.toLocaleString("en-US")}.${frac}`;
}

/** Milliseconds as a compact duration: 850ms, 1.2s, 42s, 3m 07s, 1h 05m. */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const totalSeconds = ms / 1000;
  if (totalSeconds < 10) return `${totalSeconds.toFixed(1)}s`;
  const seconds = Math.floor(totalSeconds);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, "0")}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
  return `${Math.floor(hours / 24)}d ${String(hours % 24).padStart(2, "0")}h`;
}

/**
 * Milliseconds between two ISO timestamps; `end` defaults to `now` for work still in
 * progress. Returns undefined when `start` is missing or unparsable.
 */
export function elapsedMs(start?: string, end?: string, now: number = Date.now()): number | undefined {
  if (!start) return undefined;
  const s = Date.parse(start);
  if (Number.isNaN(s)) return undefined;
  const e = end ? Date.parse(end) : now;
  if (Number.isNaN(e)) return undefined;
  return Math.max(0, e - s);
}

/**
 * Identifiers are UUIDv7: the leading characters are a timestamp shared by everything
 * created in the same minute, so the short form uses the trailing (random) characters.
 */
export function shortId(id: string | null | undefined, length = 8): string {
  if (!id) return "—";
  const compact = id.replace(/-/g, "");
  return compact.length > length ? compact.slice(-length) : compact;
}

/** 12345 becomes "12,345". */
export function formatCount(n: number | null | undefined): string {
  return (n ?? 0).toLocaleString("en-US");
}

const dateTime = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

/** "Oct 15, 11:34:07" in the viewer's time zone, or an em dash for a missing value. */
export function formatDateTime(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  return Number.isNaN(t) ? iso : dateTime.format(t);
}

/** "waiting_approval" becomes "waiting approval". */
export function humanize(value: string): string {
  return value.replace(/_/g, " ");
}
