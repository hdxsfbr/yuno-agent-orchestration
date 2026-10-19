import type { AgentMessage, Approval, ModelCall, Run, RunBranch, RunNode, SSEEvent, ToolCall, Trace } from "./api";

// The run page keeps the trace (GET /api/runs/{id}/trace) and applies the run's SSE events
// on top of it. Every event carries the complete object, so applying one is an upsert by
// id; applying the same event twice, or replaying older events after a reconnect, leaves
// the same state once the newest event for each object has been seen.

export type LogLine = { id?: string; level: string; message: string; at?: string };

/** One retry of the run from the step it stopped at (the `run.retry` event, DESIGN 5.21); `retries` is its number, counted from 1. */
export type RetryMark = { retries: number; from_node: string; by?: string; at?: string };

/** The trace as the run page holds it; `branches` is empty for a run without forks (DESIGN 5.26). */
export type RunView = Omit<Trace, "branches"> & { branches: RunBranch[]; logs: LogLine[]; retries: RetryMark[] };

/** Log lines kept in memory; older lines are dropped from the front. */
export const MAX_LOG_LINES = 500;

/** Log lines and retry marks exist only on the event stream, so a reloaded trace brings them over from the view it replaces. */
export function toRunView(trace: Trace, logs: LogLine[] = [], retries: RetryMark[] = []): RunView {
  return { ...trace, branches: trace.branches ?? [], logs, retries };
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
    case "branch.status": {
      const branch = pick<RunBranch>(ev.data, "branch");
      if (!branch || branch.run_id !== runId) return view;
      return { ...view, branches: upsert(view.branches, branch) };
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

/** One branch of a fork card: the branch row and the node visits made inside it, in the order they started. */
export type TimelineBranch = { branch: RunBranch; groups: TimelineNode[] };

/**
 * What the timeline lists: a node visit with its calls, a fork visit with its branches side by side, the join
 * that ended a fork visit, or the boundary of a retry between two of them.
 */
export type TimelineEntry =
  | { kind: "node"; group: TimelineNode }
  | { kind: "fork"; group: TimelineNode; branches: TimelineBranch[] }
  | { kind: "join"; group: TimelineNode; branches: number; fork: RunNode }
  | { kind: "retry"; mark: RetryMark };

/** The heads a fork or join row names in its output, `{"branches": [...]}` (DESIGN 5.26 rules 3 and 5). */
function headsIn(output: unknown): string[] | undefined {
  if (!isRecord(output)) return undefined;
  const keys = Object.keys(output);
  const heads = output.branches;
  if (keys.length !== 1 || !Array.isArray(heads) || !heads.every((h) => typeof h === "string")) return undefined;
  return heads as string[];
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  const x = new Set(a);
  const y = new Set(b);
  return x.size === y.size && [...x].every((h) => y.has(h));
}

/**
 * The branches of a fork visit in the order of the fork's edges, which its row records as `output.branches`; branches
 * the row does not name follow in the order they started.
 */
function inEdgeOrder(fork: RunNode, branches: readonly RunBranch[]): RunBranch[] {
  const heads = headsIn(fork.output) ?? [];
  const rank = (b: RunBranch) => {
    const i = heads.indexOf(b.head);
    return i < 0 ? Number.POSITIVE_INFINITY : i;
  };
  const started = (b: RunBranch) => {
    const t = Date.parse(b.started_at);
    return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
  };
  return branches
    .map((branch, index) => ({ branch, index }))
    .sort((a, b) => rank(a.branch) - rank(b.branch) || started(a.branch) - started(b.branch) || a.index - b.index)
    .map((x) => x.branch);
}

/**
 * The timeline groups with a marker for each retry placed before the first visit that started
 * at or after it, so the visit that failed ends above the marker and the new visit starts
 * below it. A retry with no readable time goes after the visits that started.
 *
 * A fork visit (the row `fork_visit_id` of one or more branches) is one entry that holds its
 * branches, each with the node visits made inside it (`branch_id`); a visit whose branch or fork
 * row the view does not hold yet stays an ordinary entry. The join that ended a fork visit, the
 * row without an agent whose output lists exactly that visit's heads, follows it as an entry of
 * its own that holds the fork visit it ended.
 */
export function buildTimelineEntries(
  view: Pick<RunView, "nodes" | "model_calls" | "tool_calls" | "approvals" | "retries"> & { branches?: readonly RunBranch[] },
): TimelineEntry[] {
  const marks = [...view.retries].sort((a, b) => a.retries - b.retries);
  const timeOf = (iso?: string) => {
    const t = iso ? Date.parse(iso) : NaN;
    return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
  };

  // The branches a fork card can hold: those whose fork row the view has.
  const rowIds = new Set(view.nodes.map((n) => n.id));
  const forkBranches = new Map<string, RunBranch[]>();
  const branchById = new Map<string, RunBranch>();
  for (const b of view.branches ?? []) {
    if (!rowIds.has(b.fork_visit_id)) continue;
    branchById.set(b.id, b);
    const list = forkBranches.get(b.fork_visit_id);
    if (list) list.push(b);
    else forkBranches.set(b.fork_visit_id, [b]);
  }

  const inside = new Map<string, TimelineNode[]>();
  const top: TimelineNode[] = [];
  for (const group of buildTimeline(view)) {
    const bid = group.node?.branch_id;
    if (bid && branchById.has(bid)) {
      const list = inside.get(bid);
      if (list) list.push(group);
      else inside.set(bid, [group]);
    } else top.push(group);
  }

  const out: TimelineEntry[] = [];
  const open: { heads: string[]; fork: RunNode }[] = [];
  let next = 0;
  for (const group of top) {
    const started = group.node?.started_at ? timeOf(group.node.started_at) : Number.POSITIVE_INFINITY;
    while (next < marks.length && timeOf(marks[next].at) <= started) out.push({ kind: "retry", mark: marks[next++] });
    const row = group.node;
    const owned = row ? forkBranches.get(row.id) : undefined;
    if (row && owned) {
      const ordered = inEdgeOrder(row, owned);
      open.push({ heads: ordered.map((b) => b.head), fork: row });
      out.push({ kind: "fork", group, branches: ordered.map((branch) => ({ branch, groups: inside.get(branch.id) ?? [] })) });
      continue;
    }
    const joined = row && !row.agent_id && !row.branch_id ? headsIn(row.output) : undefined;
    // The newest fork visit still open whose heads the row lists is the one it joins.
    let fork = -1;
    if (joined) for (let i = open.length - 1; i >= 0 && fork < 0; i--) if (sameSet(open[i].heads, joined)) fork = i;
    if (joined && fork >= 0) {
      const [visit] = open.splice(fork, 1);
      out.push({ kind: "join", group, branches: joined.length, fork: visit.fork });
      continue;
    }
    out.push({ kind: "node", group });
  }
  while (next < marks.length) out.push({ kind: "retry", mark: marks[next++] });
  return out;
}

/**
 * Where a live run stands inside a fork: while `currentNode` is the fork of the newest fork visit, the fork's id, how many
 * of that visit's branches are running and how many are parked on a question. Undefined when the run is not in a fork.
 */
export function forkProgress(
  branches: readonly RunBranch[] | undefined,
  currentNode: string | undefined,
): { fork: string; running: number; waiting: number } | undefined {
  if (!currentNode) return undefined;
  const ofFork = (branches ?? []).filter((b) => b.fork_id === currentNode);
  if (ofFork.length === 0) return undefined;
  const startedAt = (b: RunBranch) => {
    const t = Date.parse(b.started_at);
    return Number.isNaN(t) ? Number.NEGATIVE_INFINITY : t;
  };
  // The newest fork visit: the one of the branch that started last (the later row wins a tie).
  let newest = ofFork[0];
  for (const b of ofFork) if (startedAt(b) >= startedAt(newest)) newest = b;
  const visit = ofFork.filter((b) => b.fork_visit_id === newest.fork_visit_id);
  return {
    fork: currentNode,
    running: visit.filter((b) => b.status === "running").length,
    waiting: visit.filter((b) => b.status === "waiting_approval").length,
  };
}
