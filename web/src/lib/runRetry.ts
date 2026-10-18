// Retrying a run from the step it stopped at (DESIGN 5.21). Pure functions so they can be unit tested.

import type { Limits, Run, ToolCall } from "./api";
import { LIMIT_FIELDS, parseFieldErrors, type FieldErrors } from "./agentForm";

/** Statuses a run can be retried from. */
const RETRYABLE = new Set<string>(["failed", "timed_out", "cancelled"]);

/** A run that ended unsuccessfully and knows the step it stopped at. */
export const canRetry = (run: Pick<Run, "status" | "current_node">): boolean => RETRYABLE.has(run.status) && !!run.current_node;

/** The limits a run is counted against: the only ones a retry can find used up. */
export const RETRY_LIMIT_KEYS = ["max_steps", "max_tool_calls", "max_cost_usd"] as const;
export type RetryLimitKey = (typeof RETRY_LIMIT_KEYS)[number];

type RetrySpec = Extract<(typeof LIMIT_FIELDS)[number], { key: RetryLimitKey }>;

/** The agent form's definitions of those three limits (label, default, whole number or not), in its order. */
export const RETRY_LIMIT_FIELDS: readonly RetrySpec[] = LIMIT_FIELDS.filter((f): f is RetrySpec => (RETRY_LIMIT_KEYS as readonly string[]).includes(f.key));

/**
 * What a 409 `limits_exhausted` names: for each exhausted limit the server's sentence
 * ("40 of 40 used"). A path may carry a prefix ("limits.max_steps") and a sentence may start
 * with the limit's own name ("max_steps: 40 of 40 used"), which the dialog says as a label
 * instead. A limit the dialog has no field for is kept under its own name so the operator
 * still reads it.
 */
export function exhaustedLimits(details: unknown): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const [path, messages] of Object.entries(parseFieldErrors(details))) {
    const key = path.split(".").pop() || path;
    (out[key] ??= []).push(...messages.map((m) => (m.startsWith(`${key}:`) ? m.slice(key.length + 1).trim() : m)));
  }
  return out;
}

export type RetryDraft = Record<RetryLimitKey, string>;

/** The cap a sentence like "40 of 40 used" or "$2.00 of $2.00 used" names (the number after "of"), as text. */
function capIn(message: string): string {
  const m = /\bof\s+\$?(\d[\d,]*(?:\.\d+)?)/i.exec(message);
  return m ? m[1].replace(/,/g, "") : "";
}

/** The fields' starting text: the run's own limits, or else the cap the server's sentence names. */
export function startingDraft(limits: Limits, exhausted: Record<string, string[]>): RetryDraft {
  const draft = { max_steps: "", max_tool_calls: "", max_cost_usd: "" } as RetryDraft;
  for (const key of RETRY_LIMIT_KEYS) {
    const own = limits[key];
    draft[key] = own ? String(own) : (exhausted[key]?.map(capIn).find(Boolean) ?? "");
  }
  return draft;
}

const INTEGER = /^\d+$/;
const DECIMAL = /^(\d+\.?\d*|\.\d+)$/;

/**
 * Checks the fields: a filled one must be a positive number (a whole one for steps and tool
 * calls), and an exhausted one must be filled and above where it started, since the same cap
 * would be refused again.
 */
export function validateRetry(draft: RetryDraft, start: RetryDraft, exhausted: ReadonlySet<string>): FieldErrors {
  const errors: FieldErrors = {};
  for (const spec of RETRY_LIMIT_FIELDS) {
    const text = draft[spec.key].trim();
    const used = exhausted.has(spec.key);
    if (text === "") {
      if (used) errors[spec.key] = [`${spec.label} is used up: enter a higher limit`];
      continue;
    }
    if (!(spec.integer ? INTEGER : DECIMAL).test(text)) {
      errors[spec.key] = [spec.integer ? `${spec.label} must be a whole number` : `${spec.label} must be a number`];
      continue;
    }
    const n = Number(text);
    if (n <= 0) {
      errors[spec.key] = [`${spec.label} must be greater than 0`];
      continue;
    }
    const before = Number(start[spec.key]);
    if (used && start[spec.key].trim() !== "" && Number.isFinite(before) && n <= before) {
      errors[spec.key] = [`${spec.label} is used up: raise it above ${start[spec.key].trim()}`];
    }
  }
  return errors;
}

/**
 * The limits to send. They replace the run's, so everything the run already had stays and the
 * three counted limits take what was typed; one left empty is dropped (the platform default).
 */
export function retryLimits(limits: Limits, draft: RetryDraft): Limits {
  const out: Limits = { ...limits };
  for (const key of RETRY_LIMIT_KEYS) {
    const text = draft[key].trim();
    if (text === "") delete out[key];
    else out[key] = Number(text);
  }
  return out;
}

/** Whether the run already did something a retry repeats: a release deployed or a GitLab request created through the API (DESIGN 5.21 rule 5). */
export function repeatsSideEffects(toolCalls: readonly Pick<ToolCall, "tool_name" | "status" | "is_error">[]): boolean {
  return toolCalls.some((c) => {
    const name = c.tool_name.replace(/^yuno__/, "");
    return (name === "deploy_release" || name === "gitlab_create_mr") && c.status === "executed" && !c.is_error;
  });
}
