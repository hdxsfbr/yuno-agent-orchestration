// Display helpers for the merge and pull requests the platform opened (DESIGN 5.20). Pure
// functions so they can be unit tested.

import type { CIStatus, MergeRequest, MergeRequestState, ProjectProvider } from "./api";
import { providerLabel } from "./project";

/** "Merge request" on GitLab, "Pull request" on GitHub. */
export const requestNoun = (provider: ProjectProvider | string): string => (provider === "github" ? "Pull request" : "Merge request");

/** "!12" on GitLab, "#12" on GitHub. */
export const requestRef = (mr: Pick<MergeRequest, "provider" | "number">): string => `${mr.provider === "github" ? "#" : "!"}${mr.number}`;

export const STATE_LABEL: Record<MergeRequestState, string> = { open: "Open", merged: "Merged", closed: "Closed" };
export const CI_LABEL: Record<CIStatus, string> = { none: "None", pending: "Pending", passed: "Passed", failed: "Failed" };

/** "3 unresolved threads", "1 unresolved thread", "No unresolved threads" or, when the provider could not tell, "Threads unknown". */
export function threadsText(n: number | null | undefined): string {
  if (n == null) return "Threads unknown";
  if (n === 0) return "No unresolved threads";
  return `${n} unresolved ${n === 1 ? "thread" : "threads"}`;
}

/** Why the request cannot be merged from here, or undefined when it can: it must be open and its pipeline passed or absent. */
export function mergeBlockedReason(mr: Pick<MergeRequest, "state" | "ci_status" | "provider">): string | undefined {
  if (mr.state !== "open") return `The request is already ${mr.state}.`;
  if (mr.ci_status === "pending") return "The pipeline is still running.";
  if (mr.ci_status === "failed") return `The pipeline failed. Merge on ${providerLabel(mr.provider)} to override.`;
  return undefined;
}

/** True when the record was never refreshed or last was longer ago than `maxAgeMs`. */
export function isStale(mr: Pick<MergeRequest, "synced_at">, now: number, maxAgeMs: number): boolean {
  if (!mr.synced_at) return true;
  const t = Date.parse(mr.synced_at);
  return Number.isNaN(t) || now - t > maxAgeMs;
}
