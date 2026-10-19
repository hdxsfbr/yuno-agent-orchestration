import { describe, expect, it } from "vitest";
import type { AgentMessage, Approval, MergeRequest, ModelCall, Run, RunBranch, RunNode, ToolCall, Trace } from "./api";
import { MAX_LOG_LINES, applyEvent, applyEvents, buildTimeline, buildTimelineEntries, forkProgress, toRunView } from "./trace";

const RUN = "run-1";

const run = (over: Partial<Run> = {}): Run => ({
  id: RUN,
  workflow_id: "wf-1",
  workflow_version: 1,
  trigger: "manual",
  status: "queued",
  input: {},
  state: {},
  limits: {},
  step_count: 0,
  tool_call_count: 0,
  cost_usd_micros: 0,
  created_at: "2026-10-15T10:00:00Z",
  ...over,
});

const node = (over: Partial<RunNode> = {}): RunNode => ({
  id: "n1",
  run_id: RUN,
  node_id: "agent",
  visit: 1,
  iteration: 1,
  status: "pending",
  ...over,
});

const modelCall = (over: Partial<ModelCall> = {}): ModelCall => ({
  id: "m1",
  run_id: RUN,
  run_node_id: "n1",
  provider: "anthropic",
  model: "claude-opus-5-5",
  input_tokens: 10,
  output_tokens: 5,
  cost_usd_micros: 100,
  duration_ms: 400,
  attempt: 1,
  started_at: "2026-10-15T10:00:02Z",
  ...over,
});

const toolCall = (over: Partial<ToolCall> = {}): ToolCall => ({
  id: "t1",
  run_id: RUN,
  run_node_id: "n1",
  tool_use_id: "toolu_1",
  tool_name: "read",
  input: { path: "a.txt" },
  is_error: false,
  status: "executed",
  attempt: 1,
  duration_ms: 12,
  started_at: "2026-10-15T10:00:03Z",
  ...over,
});

const approval = (over: Partial<Approval> = {}): Approval => ({
  id: "a1",
  run_id: RUN,
  run_node_id: "n1",
  kind: "tool",
  node_id: "agent",
  message: "Deploy release 3?",
  status: "pending",
  requested_at: "2026-10-15T10:00:04Z",
  expires_at: "2026-10-16T10:00:04Z",
  ...over,
});

const message = (over: Partial<AgentMessage> = {}): AgentMessage => ({
  id: "g1",
  run_id: RUN,
  from_node: "writer",
  to_node: "reviewer",
  payload: { text: "draft" },
  created_at: "2026-10-15T10:00:05Z",
  ...over,
});

const branch = (over: Partial<RunBranch> = {}): RunBranch => ({
  id: "b1",
  run_id: RUN,
  fork_visit_id: "f1",
  fork_id: "panel",
  head: "security",
  status: "running",
  current_node: "security",
  started_at: "2026-10-15T10:00:10Z",
  ...over,
});

const emptyTrace = (r: Run = run()): Trace => ({
  run: r,
  nodes: [],
  model_calls: [],
  tool_calls: [],
  spans: [],
  agent_messages: [],
  approvals: [],
});

const base = () => toRunView(emptyTrace());
const ev = (event: string, data: unknown, id?: string) => ({ event, data, id });

describe("applyEvent", () => {
  it("replaces the run on run.status", () => {
    const next = applyEvent(base(), ev("run.status", { run: run({ status: "running", step_count: 2 }) }));
    expect(next.run.status).toBe("running");
    expect(next.run.step_count).toBe(2);
  });

  it("adds a node, then updates it in place", () => {
    let v = applyEvent(base(), ev("node.status", { node: node({ status: "running", started_at: "2026-10-15T10:00:01Z" }) }));
    expect(v.nodes).toHaveLength(1);
    v = applyEvent(v, ev("node.status", { node: node({ status: "succeeded", output: { answer: 42 }, finished_at: "2026-10-15T10:00:09Z" }) }));
    expect(v.nodes).toHaveLength(1);
    expect(v.nodes[0].status).toBe("succeeded");
    expect(v.nodes[0].output).toEqual({ answer: 42 });
  });

  it("adds a branch on branch.status, then updates it in place", () => {
    let v = applyEvent(base(), ev("branch.status", { branch: branch() }));
    expect(v.branches).toEqual([branch()]);
    v = applyEvent(v, ev("branch.status", { branch: branch({ id: "b2", head: "performance" }) }));
    v = applyEvent(v, ev("branch.status", { branch: branch({ status: "failed", failure_reason: "boom", finished_at: "2026-10-15T10:00:20Z" }) }));
    expect(v.branches.map((b) => [b.id, b.status])).toEqual([
      ["b1", "failed"],
      ["b2", "running"],
    ]);
    expect(v.branches[0].failure_reason).toBe("boom");
    // the same event twice leaves the same state
    expect(applyEvent(v, ev("branch.status", { branch: branch({ status: "failed", failure_reason: "boom", finished_at: "2026-10-15T10:00:20Z" }) })).branches).toEqual(v.branches);
  });

  it("ignores a branch of another run and a branch event without a branch", () => {
    const v = base();
    expect(applyEvent(v, ev("branch.status", { branch: branch({ run_id: "other" }) }))).toBe(v);
    expect(applyEvent(v, ev("branch.status", {}))).toBe(v);
    expect(applyEvent(v, ev("branch.status", { branch: { head: "security" } }))).toBe(v);
    expect(applyEvent(v, ev("branch.status", "nope"))).toBe(v);
  });

  it("upserts model calls and tool calls by id", () => {
    let v = applyEvent(base(), ev("model_call", { model_call: modelCall() }));
    v = applyEvent(v, ev("model_call", { model_call: modelCall({ output_tokens: 99, stop_reason: "tool_use" }) }));
    v = applyEvent(v, ev("model_call", { model_call: modelCall({ id: "m2" }) }));
    expect(v.model_calls.map((m) => m.id)).toEqual(["m1", "m2"]);
    expect(v.model_calls[0].output_tokens).toBe(99);

    v = applyEvent(v, ev("tool_call", { tool_call: toolCall({ status: "awaiting_approval" }) }));
    v = applyEvent(v, ev("tool_call", { tool_call: toolCall({ status: "executed", output: "ok" }) }));
    expect(v.tool_calls).toHaveLength(1);
    expect(v.tool_calls[0]).toMatchObject({ status: "executed", output: "ok" });
  });

  it("takes the run counters a call event carries, never moving them backwards", () => {
    const counters = { step_count: 2, tool_call_count: 1, input_tokens: 300, output_tokens: 40, cost_usd_micros: 700 };
    let v = applyEvent(base(), ev("model_call", { model_call: modelCall(), run: run({ status: "running", ...counters }) }));
    expect(v.run).toMatchObject(counters);
    expect(v.run.status).toBe("queued"); // the status still comes from run.status only
    v = applyEvent(v, ev("tool_call", { tool_call: toolCall(), run: run({ ...counters, tool_call_count: 2 }) }));
    expect(v.run.tool_call_count).toBe(2);
    const replayed = applyEvent(v, ev("model_call", { model_call: modelCall(), run: run({ step_count: 1, cost_usd_micros: 100 }) }));
    expect(replayed.run).toMatchObject({ ...counters, tool_call_count: 2 });
    expect(applyEvent(v, ev("model_call", { model_call: modelCall(), run: run({ id: "other", step_count: 9 }) })).run.step_count).toBe(2);
  });

  it("records a requested approval, then its decision", () => {
    let v = applyEvent(base(), ev("approval.requested", { approval: approval() }));
    expect(v.approvals[0].status).toBe("pending");
    v = applyEvent(v, ev("approval.decided", { approval: approval({ status: "approved", decided_at: "2026-10-15T10:01:00Z", decided_by: "andre" }) }));
    expect(v.approvals).toHaveLength(1);
    expect(v.approvals[0]).toMatchObject({ status: "approved", decided_by: "andre" });
  });

  it("appends agent messages once", () => {
    let v = applyEvent(base(), ev("agent_message", { agent_message: message() }));
    v = applyEvent(v, ev("agent_message", { agent_message: message() }));
    v = applyEvent(v, ev("agent_message", { agent_message: message({ id: "g2" }) }));
    expect(v.agent_messages.map((m) => m.id)).toEqual(["g1", "g2"]);
  });

  it("appends log lines in order and skips a replayed one", () => {
    let v = applyEvent(base(), ev("log", { run_id: RUN, level: "info", message: "starting" }, "10"));
    v = applyEvent(v, ev("log", { run_id: RUN, level: "warn", message: "slow tool" }, "11"));
    v = applyEvent(v, ev("log", { run_id: RUN, level: "warn", message: "slow tool" }, "11"));
    expect(v.logs).toEqual([
      { id: "10", level: "info", message: "starting", at: undefined },
      { id: "11", level: "warn", message: "slow tool", at: undefined },
    ]);
  });

  it("keeps only the newest log lines", () => {
    let v = base();
    for (let i = 0; i < MAX_LOG_LINES + 5; i++) v = applyEvent(v, ev("log", { run_id: RUN, level: "info", message: `line ${i}` }, String(i + 1)));
    expect(v.logs).toHaveLength(MAX_LOG_LINES);
    expect(v.logs[0].message).toBe("line 5");
    expect(v.logs.at(-1)?.message).toBe(`line ${MAX_LOG_LINES + 4}`);
  });

  it("records a retry from run.retry", () => {
    const v = applyEvent(base(), ev("run.retry", { retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" }, "20"));
    expect(v.retries).toEqual([{ retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" }]);
    expect(base().retries).toEqual([]);
  });

  it("keeps one mark per retry number, the later event replacing the earlier, in number order", () => {
    let v = applyEvent(base(), ev("run.retry", { retries: 2, from_node: "reviewer", at: "2026-10-15T10:20:00Z" }));
    v = applyEvent(v, ev("run.retry", { retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" }));
    v = applyEvent(v, ev("run.retry", { retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" }));
    expect(v.retries.map((r) => [r.retries, r.from_node, r.by])).toEqual([
      [1, "coder", "operator"],
      [2, "reviewer", undefined],
    ]);
    expect(applyEvents(v, [ev("run.retry", { retries: 1, from_node: "coder", by: "operator", at: "2026-10-15T10:06:00Z" })]).retries).toEqual(v.retries);
  });

  it("ignores a retry event for another run or without a number or a node", () => {
    const v = base();
    expect(applyEvent(v, ev("run.retry", { run_id: "other", retries: 1, from_node: "coder" }))).toBe(v);
    expect(applyEvent(v, ev("run.retry", { retries: 0, from_node: "coder" }))).toBe(v);
    expect(applyEvent(v, ev("run.retry", { retries: 1.5, from_node: "coder" }))).toBe(v);
    expect(applyEvent(v, ev("run.retry", { retries: "1", from_node: "coder" }))).toBe(v);
    expect(applyEvent(v, ev("run.retry", { retries: 1 }))).toBe(v);
    expect(applyEvent(v, ev("run.retry", "nope"))).toBe(v);
    expect(applyEvent(base(), ev("run.retry", { run_id: RUN, retries: 1, from_node: "coder" })).retries).toHaveLength(1);
  });

  it("keeps the merge request the view has when a run.status event carries the run without one", () => {
    const record = { id: "mr1", run_id: RUN, number: 12 } as MergeRequest;
    const loaded = toRunView(emptyTrace(run({ merge_request: record })));
    expect(applyEvent(loaded, ev("run.status", { run: run({ status: "running" }) })).run.merge_request).toBe(record);
    const newer = { ...record, number: 13 } as MergeRequest;
    expect(applyEvent(loaded, ev("run.status", { run: run({ merge_request: newer }) })).run.merge_request).toBe(newer);
    expect(applyEvent(base(), ev("run.status", { run: run() })).run.merge_request).toBeUndefined();
  });

  it("ignores events for another run, unknown events and malformed payloads", () => {
    const v = base();
    expect(applyEvent(v, ev("run.status", { run: run({ id: "other", status: "failed" }) }))).toBe(v);
    expect(applyEvent(v, ev("node.status", { node: node({ run_id: "other" }) }))).toBe(v);
    expect(applyEvent(v, ev("log", { run_id: "other", level: "info", message: "x" }))).toBe(v);
    expect(applyEvent(v, ev("heartbeat", {}))).toBe(v);
    expect(applyEvent(v, ev("node.status", "not an object"))).toBe(v);
    expect(applyEvent(v, ev("node.status", { node: { status: "running" } }))).toBe(v);
  });

  it("does not change its input", () => {
    const v = base();
    applyEvent(v, ev("node.status", { node: node() }));
    expect(v.nodes).toEqual([]);
  });
});

describe("replay", () => {
  const history = [
    ev("run.status", { run: run({ status: "running" }) }, "1"),
    ev("node.status", { node: node({ status: "running", started_at: "2026-10-15T10:00:01Z" }) }, "2"),
    ev("model_call", { model_call: modelCall() }, "3"),
    ev("log", { run_id: RUN, level: "info", message: "hello" }, "4"),
    ev("node.status", { node: node({ status: "succeeded", finished_at: "2026-10-15T10:00:09Z" }) }, "5"),
    ev("run.status", { run: run({ status: "succeeded" }) }, "6"),
  ];

  it("reaches the same state when events are applied twice", () => {
    const once = applyEvents(base(), history);
    const twice = applyEvents(once, history);
    expect(twice).toEqual(once);
    expect(once.run.status).toBe("succeeded");
    expect(once.nodes[0].status).toBe("succeeded");
  });

  it("applies events on top of a loaded trace", () => {
    const loaded = toRunView({ ...emptyTrace(run({ status: "running" })), nodes: [node({ status: "running" })] }, []);
    const v = applyEvents(loaded, history.slice(4));
    expect(v.nodes[0].status).toBe("succeeded");
    expect(v.run.status).toBe("succeeded");
  });
});

describe("buildTimeline", () => {
  it("groups model calls, tool calls and approvals under their node in time order", () => {
    const nodes = [node({ id: "n2", node_id: "reviewer", started_at: "2026-10-15T10:01:00Z" }), node({ started_at: "2026-10-15T10:00:01Z" })];
    const groups = buildTimeline({
      nodes,
      model_calls: [modelCall({ started_at: "2026-10-15T10:00:05Z" })],
      tool_calls: [toolCall({ started_at: "2026-10-15T10:00:03Z" }), toolCall({ id: "t2", run_node_id: "n2" })],
      approvals: [approval({ requested_at: "2026-10-15T10:00:04Z" })],
    });
    expect(groups.map((g) => g.node?.id)).toEqual(["n1", "n2"]);
    expect(groups[0].items.map((i) => i.key)).toEqual(["t:t1", "a:a1", "m:m1"]);
    expect(groups[1].items.map((i) => i.key)).toEqual(["t:t2"]);
  });

  it("puts nodes that have not started after started ones and keeps arrival order among them", () => {
    const groups = buildTimeline({
      nodes: [node({ id: "p1", node_id: "later" }), node({ id: "p2", node_id: "latest" }), node({ id: "s1", node_id: "first", started_at: "2026-10-15T10:00:01Z" })],
      model_calls: [],
      tool_calls: [],
      approvals: [],
    });
    expect(groups.map((g) => g.node?.node_id)).toEqual(["first", "later", "latest"]);
  });

  it("collects items whose node is unknown under a null node", () => {
    const groups = buildTimeline({ nodes: [], model_calls: [modelCall({ run_node_id: "ghost" })], tool_calls: [], approvals: [] });
    expect(groups).toHaveLength(1);
    expect(groups[0].node).toBeNull();
    expect(groups[0].items).toHaveLength(1);
  });
});

describe("toRunView", () => {
  it("brings the logs and the retry marks of the view it replaces", () => {
    const marks = [{ retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" }];
    const v = toRunView(emptyTrace(), [{ level: "info", message: "hi" }], marks);
    expect(v.logs).toHaveLength(1);
    expect(v.retries).toBe(marks);
    expect(toRunView(emptyTrace()).retries).toEqual([]);
  });

  it("carries the branches of the trace, none when the trace has none", () => {
    expect(toRunView(emptyTrace()).branches).toEqual([]);
    expect(toRunView({ ...emptyTrace(), branches: [branch()] }).branches).toEqual([branch()]);
  });
});

describe("buildTimelineEntries", () => {
  const failedThenRetried = {
    nodes: [
      node({ id: "n1", node_id: "coder", visit: 1, status: "failed", started_at: "2026-10-15T10:00:01Z" }),
      node({ id: "n2", node_id: "coder", visit: 2, status: "running", started_at: "2026-10-15T10:07:00Z" }),
    ],
    model_calls: [],
    tool_calls: [],
    approvals: [],
  };
  const kinds = (entries: ReturnType<typeof buildTimelineEntries>) =>
    entries.map((e) => (e.kind === "retry" ? `retry ${e.mark.retries}` : (e.group.node?.id ?? "other")));

  it("is the plain timeline when nothing was retried", () => {
    expect(kinds(buildTimelineEntries({ ...failedThenRetried, retries: [] }))).toEqual(["n1", "n2"]);
  });

  it("puts the marker between the failed visit and the visit that started after it", () => {
    const entries = buildTimelineEntries({ ...failedThenRetried, retries: [{ retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" }] });
    expect(kinds(entries)).toEqual(["n1", "retry 1", "n2"]);
  });

  it("puts the marker before a visit that has not started yet, and last when no visit followed", () => {
    const waiting = { ...failedThenRetried, nodes: [failedThenRetried.nodes[0], node({ id: "n3", node_id: "coder", visit: 2 })] };
    expect(kinds(buildTimelineEntries({ ...waiting, retries: [{ retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" }] }))).toEqual(["n1", "retry 1", "n3"]);
    const only = { nodes: [failedThenRetried.nodes[0]], model_calls: [], tool_calls: [], approvals: [] };
    expect(kinds(buildTimelineEntries({ ...only, retries: [{ retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" }] }))).toEqual(["n1", "retry 1"]);
  });

  it("orders several retries by number around the visits and puts one without a readable time last", () => {
    const three = {
      ...failedThenRetried,
      nodes: [...failedThenRetried.nodes, node({ id: "n3", node_id: "coder", visit: 3, started_at: "2026-10-15T10:30:00Z" })],
    };
    const entries = buildTimelineEntries({
      ...three,
      retries: [
        { retries: 2, from_node: "coder", at: "2026-10-15T10:20:00Z" },
        { retries: 1, from_node: "coder", at: "2026-10-15T10:06:00Z" },
        { retries: 3, from_node: "coder" },
      ],
    });
    expect(kinds(entries)).toEqual(["n1", "retry 1", "n2", "retry 2", "n3", "retry 3"]);
  });
});

describe("buildTimelineEntries with a fork", () => {
  const at = (s: number) => `2026-10-15T10:00:${String(s).padStart(2, "0")}Z`;
  const fork = (over: Partial<RunNode> = {}) =>
    node({ id: "f1", node_id: "panel", visit: 1, status: "succeeded", started_at: at(10), finished_at: at(40), output: { branches: ["security", "performance"] }, ...over });
  const inBranch = (id: string, nodeId: string, branchId: string, start: number, over: Partial<RunNode> = {}) =>
    node({ id, node_id: nodeId, branch_id: branchId, visit: 1, status: "succeeded", agent_id: `a-${nodeId}`, started_at: at(start), ...over });
  const join = (over: Partial<RunNode> = {}) =>
    node({ id: "j1", node_id: "merge", visit: 1, status: "succeeded", started_at: at(40), finished_at: at(40), output: { branches: ["security", "performance"] }, ...over });
  const coder = () => node({ id: "n0", node_id: "coder", agent_id: "a-coder", status: "succeeded", started_at: at(1) });
  const lead = () => node({ id: "n9", node_id: "lead", agent_id: "a-lead", status: "running", started_at: at(41) });
  const two = [branch({ id: "b-sec", head: "security", started_at: at(10) }), branch({ id: "b-perf", head: "performance", started_at: at(10) })];
  const rest = { model_calls: [], tool_calls: [], approvals: [], retries: [] };

  it("holds a fork visit as one entry with a column for each branch, the visits of a branch inside it, and the join after it", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: two,
      nodes: [coder(), fork(), inBranch("n1", "security", "b-sec", 11), inBranch("n2", "performance", "b-perf", 11), inBranch("n3", "security_2", "b-sec", 20), join(), lead()],
    });
    expect(entries.map((e) => e.kind)).toEqual(["node", "fork", "join", "node"]);
    const f = entries[1];
    if (f.kind !== "fork") throw new Error("not a fork entry");
    expect(f.group.node?.id).toBe("f1");
    expect(f.branches.map((b) => [b.branch.head, b.groups.map((g) => g.node?.id)])).toEqual([
      ["security", ["n1", "n3"]],
      ["performance", ["n2"]],
    ]);
    const j = entries[2];
    if (j.kind !== "join") throw new Error("not a join entry");
    expect(j.group.node?.node_id).toBe("merge");
    expect(j.branches).toBe(2);
    expect(j.fork.id).toBe("f1");
  });

  it("orders the columns as the fork's edges, which its output records, whatever order the rows arrive in", () => {
    const reversed = [two[1], two[0]];
    const entries = buildTimelineEntries({ ...rest, branches: reversed, nodes: [fork()] });
    const f = entries[0];
    if (f.kind !== "fork") throw new Error("not a fork entry");
    expect(f.branches.map((b) => b.branch.head)).toEqual(["security", "performance"]);
  });

  it("falls back to the order the branches started in when the fork's row names none", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: [branch({ id: "b-late", head: "late", started_at: at(12) }), branch({ id: "b-early", head: "early", started_at: at(10) })],
      nodes: [fork({ output: undefined })],
    });
    const f = entries[0];
    if (f.kind !== "fork") throw new Error("not a fork entry");
    expect(f.branches.map((b) => b.branch.head)).toEqual(["early", "late"]);
  });

  it("keeps a branch with no visits yet, and a visit whose branch or fork row the view lacks as an ordinary entry", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: [...two, branch({ id: "b-ghost", fork_visit_id: "gone", head: "ghost" })],
      nodes: [fork(), inBranch("n1", "security", "b-sec", 11), inBranch("n7", "ghost", "b-ghost", 12), inBranch("n8", "stray", "no-such-branch", 13)],
    });
    expect(entries.map((e) => (e.kind === "fork" ? "fork" : e.kind === "node" ? (e.group.node?.id ?? "other") : e.kind))).toEqual(["fork", "n7", "n8"]);
    const f = entries[0];
    if (f.kind !== "fork") throw new Error("not a fork entry");
    expect(f.branches.map((b) => b.groups.length)).toEqual([1, 0]);
  });

  it("treats a fork row without branches, and a row that merely resembles a join, as ordinary entries", () => {
    const lone = buildTimelineEntries({ ...rest, branches: [], nodes: [fork()] });
    expect(lone.map((e) => e.kind)).toEqual(["node"]);
    // the join row is only a join while a fork visit lists the same heads
    const odd = buildTimelineEntries({
      ...rest,
      branches: two,
      nodes: [fork(), join({ output: { branches: ["x", "y"] } }), join({ id: "j2", agent_id: "a1", started_at: at(41), finished_at: at(41) })],
    });
    expect(odd.map((e) => e.kind)).toEqual(["fork", "node", "node"]);
  });

  it("gives a loop around the pair its own fork entry and join entry each time", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: [...two, branch({ id: "b-sec2", fork_visit_id: "f2", head: "security", started_at: at(52) }), branch({ id: "b-perf2", fork_visit_id: "f2", head: "performance", started_at: at(52) })],
      nodes: [
        fork(),
        inBranch("n1", "security", "b-sec", 11),
        join(),
        fork({ id: "f2", visit: 2, started_at: at(52), finished_at: undefined, status: "running" }),
        inBranch("n4", "security", "b-sec2", 53, { visit: 2 }),
        inBranch("n5", "performance", "b-perf2", 53, { visit: 2 }),
      ],
    });
    expect(entries.map((e) => e.kind)).toEqual(["fork", "join", "fork"]);
    const second = entries[2];
    if (second.kind !== "fork") throw new Error("not a fork entry");
    expect(second.branches.map((b) => b.groups.map((g) => g.node?.id))).toEqual([["n4"], ["n5"]]);
  });

  it("tells each join entry which fork visit it ended when the pair loops", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: [...two, branch({ id: "b-sec2", fork_visit_id: "f2", head: "security", started_at: at(52) }), branch({ id: "b-perf2", fork_visit_id: "f2", head: "performance", started_at: at(52) })],
      nodes: [
        fork(),
        join(),
        fork({ id: "f2", visit: 2, started_at: at(52), finished_at: at(58) }),
        join({ id: "j2", visit: 2, started_at: at(58), finished_at: at(58) }),
      ],
    });
    expect(entries.map((e) => e.kind)).toEqual(["fork", "join", "fork", "join"]);
    expect(entries.flatMap((e) => (e.kind === "join" ? [[e.group.node?.id, e.fork.id, e.fork.visit]] : []))).toEqual([
      ["j1", "f1", 1],
      ["j2", "f2", 2],
    ]);
  });

  it("places a retry mark after the fork card it follows and before the join", () => {
    const entries = buildTimelineEntries({
      ...rest,
      branches: two,
      nodes: [fork(), inBranch("n1", "security", "b-sec", 11), join()],
      retries: [{ retries: 1, from_node: "security", at: at(30) }],
    });
    expect(entries.map((e) => (e.kind === "retry" ? "retry" : e.kind))).toEqual(["fork", "retry", "join"]);
  });

  it("is the plain timeline for a run without branches", () => {
    const entries = buildTimelineEntries({ ...rest, nodes: [coder(), lead()] });
    expect(entries.map((e) => e.kind)).toEqual(["node", "node"]);
  });
});

describe("forkProgress", () => {
  const two = [branch({ id: "b1", head: "security" }), branch({ id: "b2", head: "performance", status: "succeeded" }), branch({ id: "b3", head: "maintainability", status: "waiting_approval" })];

  it("counts the running branches and the ones waiting for an answer apart when the run is at the fork", () => {
    expect(forkProgress(two, "panel")).toEqual({ fork: "panel", running: 1, waiting: 1 });
  });

  it("is undefined when the run is elsewhere or has no branches", () => {
    expect(forkProgress(two, "coder")).toBeUndefined();
    expect(forkProgress(two, undefined)).toBeUndefined();
    expect(forkProgress([], "panel")).toBeUndefined();
    expect(forkProgress(undefined, "panel")).toBeUndefined();
  });

  it("counts only the newest fork visit of the fork", () => {
    const loop = [
      ...two.map((b) => ({ ...b, status: "succeeded" as const })),
      branch({ id: "b4", fork_visit_id: "f2", head: "security", started_at: "2026-10-15T10:05:00Z" }),
      branch({ id: "b5", fork_visit_id: "f2", head: "performance", started_at: "2026-10-15T10:05:00Z" }),
    ];
    expect(forkProgress(loop, "panel")).toEqual({ fork: "panel", running: 2, waiting: 0 });
    expect(forkProgress([...loop, branch({ id: "b6", fork_visit_id: "f2", head: "third", status: "failed", started_at: "2026-10-15T10:05:00Z" })], "panel")?.running).toBe(2);
  });

  it("does not count a parked branch as running", () => {
    expect(forkProgress([branch({ status: "waiting_approval" }), branch({ id: "b2", head: "performance", status: "succeeded" })], "panel")).toEqual({ fork: "panel", running: 0, waiting: 1 });
  });

  it("says none are working once every branch has ended", () => {
    expect(forkProgress([branch({ status: "succeeded" })], "panel")).toEqual({ fork: "panel", running: 0, waiting: 0 });
  });
});
