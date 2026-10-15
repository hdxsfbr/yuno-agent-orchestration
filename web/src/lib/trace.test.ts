import { describe, expect, it } from "vitest";
import type { AgentMessage, Approval, ModelCall, Run, RunNode, ToolCall, Trace } from "./api";
import { MAX_LOG_LINES, applyEvent, applyEvents, buildTimeline, toRunView } from "./trace";

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
