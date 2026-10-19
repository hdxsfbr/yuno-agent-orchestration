// Display helpers for a project's plans, their tickets and events (DESIGN 5.29). Pure functions so they can be unit tested.

import type { MainState, Plan, PlanDetail, PlanEvent, PlanShipRun, PlanStatus, PlanTicket, PlanTicketStatus } from "./api";
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

export type WaveState = "pending" | "released" | "building" | "shipping" | "held" | "done";

export const WAVE_STATE_LABEL: Record<WaveState, string> = {
  pending: "Pending",
  released: "Released",
  building: "Building",
  shipping: "Shipping",
  held: "Held",
  done: "Done",
};

export const WAVE_STATE_TONE: Record<WaveState, Tone> = {
  pending: "zinc",
  released: "blue",
  building: "blue",
  shipping: "blue",
  held: "amber",
  done: "green",
};

export const MAIN_STATE_LABEL: Record<MainState, string> = {
  passed: "Passing",
  failed: "Failing",
  running: "Running",
  pending: "Pending",
  none: "No checks",
};

export const MAIN_STATE_TONE: Record<MainState, Tone> = {
  passed: "green",
  failed: "red",
  running: "blue",
  pending: "amber",
  none: "zinc",
};

/** One wave of a plan: its tickets, its state, its ship run and what the operator is asked for, if anything (DESIGN 5.29 rules 9 to 15). */
export type WaveView = {
  wave: number;
  tickets: PlanTicket[];
  state: WaveState;
  shipRun?: PlanShipRun;
  /** The ship run waits on an approval; which one is read from the run's pending approval (see `waitingFor`). */
  waiting?: boolean;
};

/** The states of a ship run that ended without shipping the wave. */
const STOPPED_RUN: readonly string[] = ["failed", "cancelled", "timed_out"];

/** The waves in order. A wave is done once its ship run succeeded or the plan moved past it, shipping while its ship run is open, held once it ended without succeeding, and otherwise follows its tickets. */
export function planWaves(detail: Pick<PlanDetail, "plan" | "tickets" | "ship_runs">): WaveView[] {
  const groups = ticketsByWave(detail.tickets);
  return groups.map((g) => {
    const shipRun = detail.ship_runs?.find((r) => r.wave === g.wave);
    const passed = detail.plan.status === "done" || g.wave < detail.plan.current_wave || shipRun?.status === "succeeded";
    let state: WaveState;
    if (passed) state = "done";
    else if (shipRun && STOPPED_RUN.includes(shipRun.status)) state = "held";
    else if (shipRun) state = "shipping";
    else if (g.tickets.every((t) => t.status === "pending")) state = "pending";
    else if (g.tickets.every((t) => t.status === "pending" || t.status === "released")) state = "released";
    else state = "building";
    const waiting = !passed && shipRun?.status === "waiting_approval";
    return { wave: g.wave, tickets: g.tickets, state, shipRun, waiting };
  });
}

const WAITING_LABEL = {
  smoke_test: "Waiting for your smoke test",
  accept: "Waiting for your acceptance",
  approval: "Waiting for your approval",
} as const;

/** What a ship run waits on, from the node of its pending approval; a plain "approval" when the node is not known or not one of the ship run's. */
export function waitingLabel(nodeId?: string): string {
  if (nodeId === "smoke_test") return WAITING_LABEL.smoke_test;
  if (nodeId === "accept_feature") return WAITING_LABEL.accept;
  return WAITING_LABEL.approval;
}

/** The plan and wave a ship run belongs to: trigger "plan" with the reference "plan:<plan id>:wave:<n>". */
export function shipRunRef(run: { trigger: string; trigger_ref?: string }): { planId: string; wave: number } | undefined {
  if (run.trigger !== "plan") return undefined;
  const m = /^plan:(.+):wave:(\d+)$/.exec(run.trigger_ref?.trim() ?? "");
  return m ? { planId: m[1], wave: Number(m[2]) } : undefined;
}
