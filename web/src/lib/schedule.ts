import type { Schedule, ScheduleInput, ScheduleKind } from "./api";
import { parseFieldErrors, type FieldErrors } from "./agentForm";
import { formatDateTime } from "./format";
import { buildInput, parseWholeInput, type InputDraft, type InputField } from "./runInput";

// Form state and helpers for the schedule editor (DESIGN 5.9).

export const KIND_LABEL: Record<ScheduleKind, string> = { cron: "Repeats", delay: "Once" };

export type ScheduleFormState = {
  workflowId: string;
  kind: ScheduleKind;
  cronExpr: string;
  tz: string;
  /** A datetime-local value ("2026-10-16T09:00") in the viewer's time zone. */
  runAt: string;
  enabled: boolean;
  draft: InputDraft;
  /** The whole input as JSON, for a workflow whose schema names no fields. */
  whole: string;
};

export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** IANA zone names for a picker, with `extra` zones (the saved one, the viewer's) always present. */
export function timeZones(...extra: string[]): string[] {
  let names: string[] = [];
  try {
    names = (Intl as unknown as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    /* an engine without the list: the extras and UTC remain */
  }
  return Array.from(new Set(["UTC", ...names, ...extra.filter(Boolean)])).sort((a, b) => (a === "UTC" ? -1 : b === "UTC" ? 1 : a.localeCompare(b)));
}

const pad = (n: number) => String(n).padStart(2, "0");

/** An ISO instant as a datetime-local value in the viewer's time zone. */
export function toLocalInput(iso?: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A datetime-local value (viewer's time zone) as an ISO instant, or undefined when it is not a date. */
export function fromLocalInput(local: string): string | undefined {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(local)) return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/** What the list shows in the schedule column: the cron expression, or the one-off time. */
export function describeWhen(s: Pick<Schedule, "kind" | "cron_expr" | "run_at">): string {
  return s.kind === "cron" ? (s.cron_expr ?? "") : `At ${formatDateTime(s.run_at)}`;
}

/** The request body for a schedule as it is now, so a toggle can send the rest unchanged. */
export function scheduleToInput(s: Schedule): ScheduleInput {
  return {
    workflow_id: s.workflow_id,
    kind: s.kind,
    ...(s.kind === "cron" ? { cron_expr: s.cron_expr } : { run_at: s.run_at }),
    tz: s.tz,
    enabled: s.enabled,
    input: s.input ?? {},
  };
}

/**
 * Reads a 422 `details` list of {path, message} into field errors keyed by the path as the
 * API wrote it (`cron_expr`, `tz`, `input.task`). Other shapes fall back to the agent form's reader.
 */
export function scheduleErrors(details: unknown): FieldErrors {
  if (Array.isArray(details) && details.every((d) => d && typeof d === "object" && typeof (d as { path?: unknown }).path === "string")) {
    const out: FieldErrors = {};
    for (const d of details as { path: string; message?: string }[]) (out[d.path.replace(/\[(\d+)\]/g, ".$1")] ??= []).push(d.message ?? "Invalid value");
    return out;
  }
  return parseFieldErrors(details);
}

export type BuiltSchedule = { value?: ScheduleInput; errors: FieldErrors };

/** The request for the form, or the problems that stop it. `fields` come from the workflow's input schema. */
export function buildSchedule(f: ScheduleFormState, fields: InputField[]): BuiltSchedule {
  const errors: FieldErrors = {};
  const add = (path: string, message: string) => (errors[path] ??= []).push(message);

  if (!f.workflowId) add("workflow_id", "A workflow is required");
  if (!f.tz.trim()) add("tz", "Choose a time zone");
  let runAt: string | undefined;
  if (f.kind === "cron") {
    if (!f.cronExpr.trim()) add("cron_expr", "Enter a cron expression");
  } else {
    runAt = fromLocalInput(f.runAt);
    if (!runAt) add("run_at", "Choose the date and time to run");
  }

  let input: Record<string, unknown> = {};
  if (fields.length === 0) {
    const parsed = parseWholeInput(f.whole);
    if (parsed.error || !parsed.input) add("input", parsed.error ?? "The input must be a JSON object");
    else input = parsed.input;
  } else {
    const built = buildInput(fields, f.draft);
    for (const [name, message] of Object.entries(built.errors)) add(`input.${name}`, message);
    input = built.input;
  }

  if (Object.keys(errors).length > 0) return { errors };
  return {
    errors,
    value: {
      workflow_id: f.workflowId,
      kind: f.kind,
      ...(f.kind === "cron" ? { cron_expr: f.cronExpr.trim() } : { run_at: runAt }),
      tz: f.tz,
      enabled: f.enabled,
      input,
    },
  };
}
