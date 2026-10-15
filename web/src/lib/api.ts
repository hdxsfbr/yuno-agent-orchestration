// Thin fetch wrapper matching the API's one error shape, plus an SSE client built on
// fetch so it can send headers (Last-Event-ID, Authorization) that EventSource cannot.

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details?: unknown,
  ) {
    super(message);
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let body: unknown = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = text;
  }
  if (!res.ok) {
    const err = (body as { error?: { code?: string; message?: string; details?: unknown } })?.error;
    throw new ApiError(res.status, err?.code ?? "http_error", err?.message ?? res.statusText, err?.details);
  }
  return body as T;
}

export type SSEEvent = { id?: string; event: string; data: unknown };

/** Parses the text/event-stream wire format incrementally. */
export function createSSEParser(onEvent: (e: SSEEvent) => void) {
  let buffer = "";
  let id: string | undefined;
  let event = "message";
  let data: string[] = [];

  const dispatch = () => {
    if (data.length > 0) {
      const raw = data.join("\n");
      let parsed: unknown = raw;
      try {
        parsed = JSON.parse(raw);
      } catch {
        /* keep raw */
      }
      onEvent({ id, event, data: parsed });
    }
    event = "message";
    data = [];
  };

  return {
    feed(chunk: string) {
      buffer += chunk;
      let idx: number;
      while ((idx = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, idx).replace(/\r$/, "");
        buffer = buffer.slice(idx + 1);
        if (line === "") {
          dispatch();
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let value = colon < 0 ? "" : line.slice(colon + 1);
        if (value.startsWith(" ")) value = value.slice(1);
        if (field === "event") event = value;
        else if (field === "data") data.push(value);
        else if (field === "id") id = value;
      }
    },
    lastId: () => id,
  };
}

export type SubscribeOptions = {
  /** Resume point for the first connection; later reconnects use the last id received. */
  lastEventId?: string;
};

/**
 * Subscribes to an SSE endpoint. Reconnects with Last-Event-ID after a 1 s pause.
 * Returns an unsubscribe function.
 */
export function subscribe(
  path: string,
  onEvent: (e: SSEEvent) => void,
  onStatus?: (s: "open" | "closed") => void,
  options?: SubscribeOptions,
) {
  const controller = new AbortController();
  let lastId: string | undefined = options?.lastEventId;
  let stopped = false;

  const connect = async () => {
    while (!stopped) {
      try {
        const res = await fetch(path, {
          headers: { Accept: "text/event-stream", ...(lastId ? { "Last-Event-ID": lastId } : {}) },
          signal: controller.signal,
        });
        if (!res.ok || !res.body) throw new Error(`sse ${res.status}`);
        onStatus?.("open");
        const parser = createSSEParser((e) => {
          if (e.id) lastId = e.id;
          onEvent(e);
        });
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          parser.feed(decoder.decode(value, { stream: true }));
        }
      } catch {
        /* fall through to reconnect */
      }
      onStatus?.("closed");
      if (stopped) return;
      await new Promise((r) => setTimeout(r, 1000));
    }
  };
  void connect();
  return () => {
    stopped = true;
    controller.abort();
  };
}

// ---------------------------------------------------------------------------------------
// Types derived from api/openapi.yaml (version 0.2.0) and the endpoint functions that use
// them. Optional fields are the ones the API may omit.
// ---------------------------------------------------------------------------------------

export type Status = {
  version: string;
  uptime_s: number;
  db_ok: boolean;
  workers: number;
  sse_clients: number;
  runs: Record<string, number>;
};

export type OpenClawStatus = {
  connected: boolean;
  version?: string;
  conn_id?: string;
  scopes: string[];
  agents?: number;
};

export type Limits = {
  max_steps?: number;
  max_duration_seconds?: number;
  max_node_iterations?: number;
  max_loop_visits?: number;
  max_cost_usd?: number;
  max_tool_calls?: number;
  approval_timeout_seconds?: number;
};

export type BlockedInputPattern = { name: string; tool: string; regex: string };

export type Guardrails = {
  max_cost_usd_per_run?: number;
  max_runs_per_hour?: number;
  blocked_tools?: string[];
  blocked_input_patterns?: BlockedInputPattern[];
};

export type MemoryPolicy = {
  enabled?: boolean;
  max_facts?: number;
  fact_ttl_days?: number;
  conversation_window?: number;
  conversation_ttl_days?: number;
};

export const EXEC_SECURITY = ["deny", "allowlist", "full"] as const;
export const EXEC_ASK = ["off", "on-miss", "always"] as const;
export type ExecSecurity = (typeof EXEC_SECURITY)[number];
export type ExecAsk = (typeof EXEC_ASK)[number];

export type ExecPolicy = {
  security?: ExecSecurity;
  ask?: ExecAsk;
  ask_fallback?: ExecSecurity;
  allowlist?: string[];
};

export const MODELS = ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"] as const;
export const EFFORTS = ["low", "medium", "high", "xhigh", "max"] as const;
export const THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "adaptive", "max"] as const;
export type Effort = (typeof EFFORTS)[number];
export type ThinkingLevel = (typeof THINKING_LEVELS)[number];

export const TOOL_POLICIES = ["auto", "require_approval", "blocked"] as const;
export type ToolPolicy = (typeof TOOL_POLICIES)[number];

export type AgentInput = {
  name: string;
  role?: string;
  system_prompt?: string;
  model: string;
  effort?: Effort;
  thinking?: ThinkingLevel;
  max_tokens?: number;
  tool_allowlist?: string[];
  interaction_rules?: Record<string, ToolPolicy>;
  limits?: Limits;
  guardrails?: Guardrails;
  memory_policy?: MemoryPolicy;
  exec_policy?: ExecPolicy;
  skills?: string[];
  channels?: "telegram"[];
};

export type Agent = AgentInput & {
  id: string;
  openclaw_agent_id?: string;
  materialized_hash?: string;
  created_at: string;
  updated_at: string;
};

export type AgentRuntime = {
  openclaw_agent_id: string;
  materialized: boolean;
  in_sync: boolean;
  workspace?: string;
  config_entry?: Record<string, unknown>;
  exec_policy?: Record<string, unknown>;
  files?: string[];
};

export type ToolRisk = "read_only" | "computational" | "side_effecting";
export type ToolSource = "platform" | "openclaw";

export type Tool = {
  name: string;
  description: string;
  input_schema?: Record<string, unknown>;
  risk: ToolRisk;
  hits: "none" | "postgres" | "local_service" | "external" | "openclaw";
  timeout_seconds: number;
  max_attempts?: number;
  untrusted_output?: boolean;
  source: ToolSource;
};

export const RUN_STATUSES = ["queued", "running", "waiting_approval", "succeeded", "failed", "cancelled", "timed_out"] as const;
export type RunStatus = (typeof RUN_STATUSES)[number];
export type RunTrigger = "manual" | "schedule" | "channel" | "eval";

export type Run = {
  id: string;
  workflow_id: string;
  workflow_version: number;
  trigger: RunTrigger;
  trigger_ref?: string;
  status: RunStatus;
  input: Record<string, unknown>;
  state: Record<string, unknown>;
  limits: Limits;
  current_node?: string;
  step_count: number;
  tool_call_count: number;
  attempt?: number;
  cancel_requested?: boolean;
  failure_reason?: string;
  cost_usd_micros: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  /** Agents the run's nodes ran, first appearance first; the list endpoint fills it. */
  agent_names?: string[];
  /** Models the run's model calls used, first appearance first; the list endpoint fills it. */
  models?: string[];
  paused_total_ms?: number;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  updated_at?: string;
};

export type RunCreate = {
  workflow_id?: string;
  input?: Record<string, unknown>;
  agent_id?: string;
  task?: string;
  limits?: Limits;
};

export type NodeStatus = "pending" | "running" | "waiting_approval" | "succeeded" | "failed" | "skipped";

export type RunNode = {
  id: string;
  run_id: string;
  node_id: string;
  agent_id?: string;
  visit: number;
  iteration: number;
  status: NodeStatus;
  messages?: Record<string, unknown>[];
  output?: unknown;
  failure_reason?: string;
  session_key?: string;
  openclaw_run_id?: string;
  started_at?: string;
  finished_at?: string;
};

export type ModelCall = {
  id: string;
  run_id: string;
  run_node_id: string;
  provider: string;
  model: string;
  request_id?: string;
  stop_reason?: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  cost_usd_micros: number;
  duration_ms: number;
  attempt: number;
  error?: string;
  started_at: string;
  finished_at?: string;
};

export type ToolCallStatus = "executed" | "awaiting_approval" | "approved" | "rejected" | "blocked" | "expired" | "failed";

export type ToolCall = {
  id: string;
  run_id: string;
  run_node_id: string;
  model_call_id?: string;
  tool_use_id: string;
  tool_name: string;
  input: unknown;
  output?: unknown;
  is_error: boolean;
  status: ToolCallStatus;
  idempotency_key?: string;
  attempt: number;
  duration_ms: number;
  started_at?: string;
  finished_at?: string;
};

export type Span = {
  id: string;
  run_id: string;
  parent_id?: string;
  kind: "run" | "node" | "model_call" | "tool_call" | "approval_wait";
  name: string;
  started_at: string;
  ended_at?: string;
  attrs: Record<string, unknown>;
};

export type AgentMessage = {
  id: string;
  run_id: string;
  from_node: string;
  to_node: string;
  payload: unknown;
  created_at: string;
};

export type ApprovalStatus = "pending" | "approved" | "rejected" | "expired";

export type Approval = {
  id: string;
  run_id: string;
  run_node_id: string;
  tool_call_id?: string;
  kind: "exec" | "tool" | "node";
  node_id: string;
  message: string;
  status: ApprovalStatus;
  external_id?: string;
  requested_at: string;
  decided_at?: string;
  decided_by?: string;
  reason?: string;
  expires_at: string;
};

export type Trace = {
  run: Run;
  nodes: RunNode[];
  model_calls: ModelCall[];
  tool_calls: ToolCall[];
  spans: Span[];
  agent_messages: AgentMessage[];
  approvals: Approval[];
};

// ---------------------------------------------------------------------------------------
// Workflows (DESIGN 5.6) and approvals (DESIGN 5.3). The definition shapes mirror
// internal/workflow/def.go; members the UI does not know about travel through every
// edit untouched because the helpers in lib/workflow.ts always spread the object they
// change.
// ---------------------------------------------------------------------------------------

export type NodeKind = "start" | "agent" | "approval" | "end";
export const NODE_KINDS: readonly NodeKind[] = ["start", "agent", "approval", "end"];

export const CONDITION_OPS = ["eq", "neq", "in", "exists", "gt", "lt"] as const;
export type ConditionOp = (typeof CONDITION_OPS)[number];

/** {path, op, value}, or {all: [...]} / {any: [...]} combining other conditions. */
export type Condition = {
  path?: string;
  op?: ConditionOp;
  value?: unknown;
  all?: Condition[];
  any?: Condition[];
};

export type WorkflowNode = {
  id: string;
  kind: NodeKind;
  agent_id?: string;
  /** How a template names its agent; instantiating resolves it to agent_id. */
  agent_name?: string;
  task?: string;
  output_schema?: unknown;
  input_from?: string[];
  message?: string;
  on_reject?: string;
  on_failure?: string;
};

export type WorkflowEdge = { from: string; to: string; when?: Condition };

export type Position = { x: number; y: number };

export type WorkflowDefinition = {
  input_schema?: unknown;
  state_schema?: unknown;
  nodes: WorkflowNode[];
  edges: WorkflowEdge[];
  limits?: Limits;
  /** Where the builder drew each node; the backend keeps it with the definition. */
  ui?: { positions?: Record<string, Position> };
};

export type WorkflowInput = { name: string; description: string; definition: WorkflowDefinition };

export type Workflow = WorkflowInput & {
  id: string;
  version: number;
  template_key?: string;
  created_at: string;
  updated_at: string;
};

export type WorkflowTemplate = { key: string; name: string; description: string; definition: WorkflowDefinition };

/** One entry of a 422 `details` list. */
export type ValidationDetail = { path: string; message: string };

/** An approval as the inbox lists it: the tool call is joined for tool approvals. */
export type ApprovalItem = Approval & { tool_call?: ToolCall };

export type ApprovalDecision = { reason?: string; always?: boolean };

const withBody = (method: string, body?: unknown): RequestInit => ({
  method,
  body: body === undefined ? undefined : JSON.stringify(body),
});

const seg = encodeURIComponent;

// Go encodes nil slices as null; the UI treats every list as possibly null.
function normalizeAgent(a: Agent): Agent {
  return {
    ...a,
    tool_allowlist: a.tool_allowlist ?? [],
    interaction_rules: a.interaction_rules ?? {},
    skills: a.skills ?? [],
    channels: a.channels ?? [],
    limits: a.limits ?? {},
    guardrails: {
      ...a.guardrails,
      blocked_tools: a.guardrails?.blocked_tools ?? [],
      blocked_input_patterns: a.guardrails?.blocked_input_patterns ?? [],
    },
    memory_policy: a.memory_policy ?? {},
    exec_policy: { ...a.exec_policy, allowlist: a.exec_policy?.allowlist ?? [] },
  };
}

export function normalizeTrace(t: Trace): Trace {
  return {
    ...t,
    nodes: t.nodes ?? [],
    model_calls: t.model_calls ?? [],
    tool_calls: t.tool_calls ?? [],
    spans: t.spans ?? [],
    agent_messages: t.agent_messages ?? [],
    approvals: t.approvals ?? [],
  };
}

export const getStatus = () => api<Status>("/api/status");
export const getOpenClawStatus = () => api<OpenClawStatus>("/api/openclaw/status");

export const listAgents = async () => ((await api<Agent[] | null>("/api/agents")) ?? []).map(normalizeAgent);
export const getAgent = async (id: string) => normalizeAgent(await api<Agent>(`/api/agents/${seg(id)}`));
export const createAgent = (input: AgentInput) => api<Agent>("/api/agents", withBody("POST", input));
export const updateAgent = (id: string, input: AgentInput) => api<Agent>(`/api/agents/${seg(id)}`, withBody("PUT", input));
export const deleteAgent = (id: string) => api<void>(`/api/agents/${seg(id)}`, withBody("DELETE"));
export const materializeAgent = (id: string) => api<AgentRuntime>(`/api/agents/${seg(id)}/materialize`, withBody("POST"));
export const getAgentRuntime = (id: string) => api<AgentRuntime>(`/api/agents/${seg(id)}/runtime`);

export const listTools = async () => (await api<Tool[] | null>("/api/tools")) ?? [];

export async function listRuns(opts: { status?: RunStatus; limit?: number } = {}) {
  const q = new URLSearchParams();
  if (opts.status) q.set("status", opts.status);
  q.set("limit", String(opts.limit ?? 100));
  return (await api<Run[] | null>(`/api/runs?${q}`)) ?? [];
}
export const createRun = (body: RunCreate) => api<Run>("/api/runs", withBody("POST", body));
export const cancelRun = (id: string) => api<Run>(`/api/runs/${seg(id)}/cancel`, withBody("POST"));
export const getTrace = async (id: string) => normalizeTrace(await api<Trace>(`/api/runs/${seg(id)}/trace`));

export const isTerminalStatus = (s: RunStatus) => s === "succeeded" || s === "failed" || s === "cancelled" || s === "timed_out";

// Go encodes nil slices as null; the builder treats nodes and edges as always present.
function normalizeDefinition(d: Partial<WorkflowDefinition> | null | undefined): WorkflowDefinition {
  return { ...d, nodes: d?.nodes ?? [], edges: d?.edges ?? [] };
}

export function normalizeWorkflow(w: Workflow): Workflow {
  return { ...w, description: w.description ?? "", definition: normalizeDefinition(w.definition) };
}

export const listWorkflows = async () => ((await api<Workflow[] | null>("/api/workflows")) ?? []).map(normalizeWorkflow);
export const getWorkflow = async (id: string) => normalizeWorkflow(await api<Workflow>(`/api/workflows/${seg(id)}`));
export const createWorkflow = async (input: WorkflowInput) => normalizeWorkflow(await api<Workflow>("/api/workflows", withBody("POST", input)));
export const updateWorkflow = async (id: string, input: WorkflowInput) =>
  normalizeWorkflow(await api<Workflow>(`/api/workflows/${seg(id)}`, withBody("PUT", input)));
export const deleteWorkflow = (id: string) => api<void>(`/api/workflows/${seg(id)}`, withBody("DELETE"));
/** Resolves for a valid definition; a 422 ApiError carries the path-qualified details. */
export const validateWorkflow = (definition: WorkflowDefinition) =>
  api<{ valid: boolean }>("/api/workflows/validate", withBody("POST", { definition }));

export const listWorkflowTemplates = async () =>
  ((await api<WorkflowTemplate[] | null>("/api/workflow-templates")) ?? []).map((t) => ({ ...t, definition: normalizeDefinition(t.definition) }));
export const instantiateTemplate = async (key: string, name?: string) =>
  normalizeWorkflow(await api<Workflow>(`/api/workflow-templates/${seg(key)}/instantiate`, withBody("POST", name ? { name } : {})));

export async function listApprovals(opts: { status?: ApprovalStatus; runId?: string } = {}) {
  const q = new URLSearchParams();
  if (opts.status) q.set("status", opts.status);
  if (opts.runId) q.set("run_id", opts.runId);
  const qs = q.toString();
  return (await api<ApprovalItem[] | null>(`/api/approvals${qs ? `?${qs}` : ""}`)) ?? [];
}
export const approveApproval = (id: string, body: ApprovalDecision = {}) => api<Approval>(`/api/approvals/${seg(id)}/approve`, withBody("POST", body));
export const rejectApproval = (id: string, body: Pick<ApprovalDecision, "reason"> = {}) =>
  api<Approval>(`/api/approvals/${seg(id)}/reject`, withBody("POST", body));
