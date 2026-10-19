// Display helpers for a project's plans, their tickets and events (DESIGN 5.29). Pure functions so they can be unit tested.

import type { Plan, PlanEvent, PlanStatus, PlanTicket, PlanTicketStatus } from "./api";
import { humanize } from "./format";
import type { Tone } from "@/components/Badge";

export const PLAN_STATUS_LABEL: Record<PlanStatus, string> = {
  publishing: "Publishing tickets",
  waiting_plan_merge: "Waiting for the plan to merge",
  building: "Building",
  shipping: "Shipping",
  accepting: "Waiting for your acceptance",
  done: "Done",
  cancelled: "Cancelled",
};

export const PLAN_STATUS_TONE: Record<PlanStatus, Tone> = {
  publishing: "blue",
  waiting_plan_merge: "amber",
  building: "blue",
  shipping: "blue",
  accepting: "amber",
  done: "green",
  cancelled: "zinc",
};

export const TICKET_STATUS_LABEL: Record<PlanTicketStatus, string> = {
  pending: "Pending",
  released: "Released",
  building: "Building",
  in_review: "In review",
  merged: "Merged",
  failed: "Failed",
  closed: "Closed",
};

export const TICKET_STATUS_TONE: Record<PlanTicketStatus, Tone> = {
  pending: "zinc",
  released: "blue",
  building: "blue",
  in_review: "amber",
  merged: "green",
  failed: "red",
  closed: "zinc",
};

/** A plan that is still being worked on: a project has at most one. */
export const isOpenPlan = (p: Pick<Plan, "status">) => p.status !== "done" && p.status !== "cancelled";

/** The tickets by wave, waves ascending, tickets in the order they came. */
export function ticketsByWave(tickets: readonly PlanTicket[]): { wave: number; tickets: PlanTicket[] }[] {
  const waves = new Map<number, PlanTicket[]>();
  for (const t of tickets) waves.set(t.wave, [...(waves.get(t.wave) ?? []), t]);
  return [...waves.entries()].sort((a, b) => a[0] - b[0]).map(([wave, list]) => ({ wave, tickets: list }));
}

/** What the operator reads for an event: a sentence, and the reason it carries when it has one. */
export function eventText(e: PlanEvent): { label: string; detail?: string } {
  const wave = e.wave != null ? ` ${e.wave}` : "";
  const reason = typeof e.detail?.reason === "string" && e.detail.reason ? e.detail.reason : undefined;
  switch (e.kind) {
    case "published":
      return { label: "Tickets published" };
    case "wave_released":
      return { label: `Wave${wave} released` };
    case "smoke_test_waiting":
      return { label: `Wave${wave} is waiting for your smoke test` };
    case "wave_done":
      return { label: `Wave${wave} done` };
    case "accept_waiting":
      return { label: "The feature is waiting for your acceptance" };
    case "feature_done":
      return { label: "Feature done" };
    case "escalated":
      return { label: e.ticket_id ? `Ticket ${e.ticket_id} escalated` : "Escalated", detail: reason };
    case "cancelled":
      return { label: reason ? `Plan cancelled: ${reason}` : "Plan cancelled" };
    case "retried":
      return { label: e.ticket_id ? `Ticket ${e.ticket_id} retried` : "Ticket retried", detail: reason };
    case "paused":
      return { label: "Waves paused", detail: reason };
    case "resumed":
      return { label: "Waves resumed" };
    default:
      return { label: humanize(e.kind).replace(/^./, (c) => c.toUpperCase()), detail: reason };
  }
}
