import type { AgentMessage, Approval, ModelCall, Run, RunNode, SSEEvent, ToolCall, Trace } from "./api";

// The run page keeps the trace (GET /api/runs/{id}/trace) and applies the run's SSE events
// on top of it. Every event carries the complete object, so applying one is an upsert by
// id; applying the same event twice, or replaying older events after a reconnect, leaves
// the same state once the newest event for each object has been seen.

export type LogLine = { id?: string; level: string; message: string; at?: string };

/** One retry of the run from the step it stopped at (the `run.retry` event, DESIGN 5.21); `retries` is its number, counted from 1. */
export type RetryMark = { retries: number; from_node: string; by?: string; at?: string };

export type RunView = Trace & { logs: LogLine[]; retries: RetryMark[] };

/** Log lines kept in memory; older lines are dropped from the front. */
export const MAX_LOG_LINES = 500;

/** Log lines and retry marks exist only on the event stream, so a reloaded trace brings them over from the view it replaces. */
export function toRunView(trace: Trace, logs: LogLine[] = [], retries: RetryMark[] = []): RunView {
  return { ...trace, logs, retries };
}

type EventLike = Pick<SSEEvent, "event" | "data"> & { id?: string };

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function upsert<T extends { id: string }>(list: T[], item: T): T[] {
  const i = list.findIndex((x) => x.id === item.id);
  if (i < 0) return [...list, item];
  const next = list.slice();
  next[i] = item;
  return next;
}

/** Payload member `key` as an object with a string `id`, or undefined. */
function pick<T extends { id: string }>(data: unknown, key: string): T | undefined {
  if (!isRecord(data)) return undefined;
  const v = data[key];
  return isRecord(v) && typeof v.id === "string" ? (v as unknown as T) : undefined;
}

/** The run counters a model or tool call moves; the run summary shows them. */
const COUNTERS = [
  "step_count",
  "tool_call_count",
  "input_tokens",
  "output_tokens",
  "cache_read_tokens",
  "cache_write_tokens",
  "cost_usd_micros",
] as const;

/**
 * Takes the counters from the run a call event carries. Counters only grow, so the larger
 * value wins: replaying older events never moves the summary backwards.
 */
function mergeCounters(view: RunView, data: unknown): RunView {
  const run = pick<Run>(data, "run");
  if (!run || run.id !== view.run.id) return view;
  let next: Run | undefined;
  for (const key of COUNTERS) {
    const v = run[key];
    if (typeof v === "number" && v > (view.run[key] ?? 0)) {
      next ??= { ...view.run };
      next[key] = v;
    }
  }
  return next ? { ...view, run: next } : view;
}

/**
 * Applies one SSE event to the view. Unknown events, malformed payloads and events that
 * belong to another run return the same object, so React can skip the render.
 */
export function applyEvent(view: RunView, ev: EventLike): RunView {
  const runId = view.run.id;
  switch (ev.event) {
    case "run.status": {
      const run = pick<Run>(ev.data, "run");
      if (!run || run.id !== runId) return view;
      // The merge request is joined onto the run when it is read; the event carries the run row alone, and a record never goes away.
      const kept = run.merge_request === undefined && view.run.merge_request ? { ...run, merge_request: view.run.merge_request } : run;
      return { ...view, run: kept };
    }
    case "run.retry": {
      const d = ev.data;
      if (!isRecord(d) || typeof d.from_node !== "string") return view;
      if (typeof d.run_id === "string" && d.run_id !== runId) return view;
      if (typeof d.retries !== "number" || !Number.isInteger(d.retries) || d.retries < 1) return view;
      const mark: RetryMark = {
        retries: d.retries,
        from_node: d.from_node,
        by: typeof d.by === "string" && d.by !== "" ? d.by : undefined,
        at: typeof d.at === "string" ? d.at : undefined,
      };
      // Replayed events carry the same number: the later one replaces the earlier.
      const others = view.retries.filter((r) => r.retries !== mark.retries);
      return { ...view, retries: [...others, mark].sort((a, b) => a.retries - b.retries) };
    }
    case "node.status": {
      const node = pick<RunNode>(ev.data, "node");
      if (!node || node.run_id !== runId) return view;
      return { ...view, nodes: upsert(view.nodes, node) };
    }
    case "model_call": {
      const call = pick<ModelCall>(ev.data, "model_call");
      if (!call || call.run_id !== runId) return view;
      return mergeCounters({ ...view, model_calls: upsert(view.model_calls, call) }, ev.data);
    }
    case "tool_call": {
      const call = pick<ToolCall>(ev.data, "tool_call");
      if (!call || call.run_id !== runId) return view;
      return mergeCounters({ ...view, tool_calls: upsert(view.tool_calls, call) }, ev.data);
    }
    case "approval.requested":
    case "approval.decided": {
      const approval = pick<Approval>(ev.data, "approval");
      if (!approval || approval.run_id !== runId) return view;
      return { ...view, approvals: upsert(view.approvals, approval) };
    }
    case "agent_message": {
      const msg = pick<AgentMessage>(ev.data, "agent_message");
      if (!msg || msg.run_id !== runId) return view;
      return { ...view, agent_messages: upsert(view.agent_messages, msg) };
    }
    case "log": {
      const d = ev.data;
      if (!isRecord(d) || typeof d.message !== "string") return view;
      if (typeof d.run_id === "string" && d.run_id !== runId) return view;
      if (ev.id !== undefined && view.logs.some((l) => l.id === ev.id)) return view;
      const line: LogLine = {
        id: ev.id,
        level: typeof d.level === "string" ? d.level : "info",
        message: d.message,
        at: typeof d.at === "string" ? d.at : typeof d.ts === "string" ? d.ts : undefined,
      };
      const logs = [...view.logs, line];
      return { ...view, logs: logs.length > MAX_LOG_LINES ? logs.slice(logs.length - MAX_LOG_LINES) : logs };
    }
    default:
      return view;
  }
}

export function applyEvents(view: RunView, events: readonly EventLike[]): RunView {
  return events.reduce(applyEvent, view);
}

// ---------------------------------------------------------------------------------------
// Timeline: node cards, each with the model calls, tool calls and approvals of that visit.
// ---------------------------------------------------------------------------------------

export type TimelineItem =
  | { kind: "model_call"; at: string; key: string; call: ModelCall }
  | { kind: "tool_call"; at: string; key: string; call: ToolCall }
  | { kind: "approval"; at: string; key: string; approval: Approval };

export type TimelineNode = { node: RunNode | null; items: TimelineItem[] };

const KIND_ORDER: Record<TimelineItem["kind"], number> = { model_call: 0, tool_call: 1, approval: 2 };

function byTime(a: TimelineItem, b: TimelineItem): number {
  const ta = Date.parse(a.at);
  const tb = Date.parse(b.at);
  if (ta !== tb && !Number.isNaN(ta) && !Number.isNaN(tb)) return ta - tb;
  return KIND_ORDER[a.kind] - KIND_ORDER[b.kind];
}

/**
 * Groups the trace by node visit. Nodes are ordered by start time (nodes that have not
 * started keep their arrival order after the started ones); items inside a node are in
 * time order. Items whose node is not (yet) known are returned under a null node.
 */
export function buildTimeline(view: Pick<RunView, "nodes" | "model_calls" | "tool_calls" | "approvals">): TimelineNode[] {
  const items = new Map<string, TimelineItem[]>();
  const push = (nodeId: string, item: TimelineItem) => {
    const list = items.get(nodeId);
    if (list) list.push(item);
    else items.set(nodeId, [item]);
  };
  for (const call of view.model_calls) push(call.run_node_id, { kind: "model_call", at: call.started_at, key: `m:${call.id}`, call });
  for (const call of view.tool_calls)
    push(call.run_node_id, { kind: "tool_call", at: call.started_at ?? "", key: `t:${call.id}`, call });
  for (const approval of view.approvals)
    push(approval.run_node_id, { kind: "approval", at: approval.requested_at, key: `a:${approval.id}`, approval });

  const startKey = (n: RunNode) => (n.started_at ? Date.parse(n.started_at) : Number.POSITIVE_INFINITY);
  const nodes = view.nodes
    .map((node, index) => ({ node, index }))
    .sort((a, b) => {
      const ka = startKey(a.node);
      const kb = startKey(b.node);
      if (ka === kb || Number.isNaN(ka) || Number.isNaN(kb)) return a.index - b.index;
      return ka < kb ? -1 : 1;
    })
    .map((x) => x.node);

  const known = new Set(nodes.map((n) => n.id));
  const out: TimelineNode[] = nodes.map((node) => ({ node, items: (items.get(node.id) ?? []).sort(byTime) }));
  const orphans: TimelineItem[] = [];
  for (const [nodeId, list] of items) if (!known.has(nodeId)) orphans.push(...list);
  if (orphans.length > 0) out.push({ node: null, items: orphans.sort(byTime) });
  return out;
}

/** What the timeline lists: a node visit with its calls, or the boundary of a retry between two of them. */
export type TimelineEntry = { kind: "node"; group: TimelineNode } | { kind: "retry"; mark: RetryMark };

/**
 * The timeline groups with a marker for each retry placed before the first visit that started
 * at or after it, so the visit that failed ends above the marker and the new visit starts
 * below it. A retry with no readable time goes after the visits that started.
 */
export function buildTimelineEntries(view: Pick<RunView, "nodes" | "model_calls" | "tool_calls" | "approvals" | "retries">): TimelineEntry[] {
  const marks = [...view.retries].sort((a, b) => a.retries - b.retries);
  const timeOf = (iso?: string) => {
    const t = iso ? Date.parse(iso) : NaN;
    return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
  };
  const out: TimelineEntry[] = [];
  let next = 0;
  for (const group of buildTimeline(view)) {
    const started = group.node?.started_at ? timeOf(group.node.started_at) : Number.POSITIVE_INFINITY;
    while (next < marks.length && timeOf(marks[next].at) <= started) out.push({ kind: "retry", mark: marks[next++] });
    out.push({ kind: "node", group });
  }
  while (next < marks.length) out.push({ kind: "retry", mark: marks[next++] });
  return out;
}
