// Thin fetch wrapper matching the API's one error shape, plus an SSE client built on
// fetch so it can send headers (Last-Event-ID, Authorization) that EventSource cannot.

import { authHeaders, getToken, reportUnauthorized, subscribeAuth } from "./auth";

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
  const sent = getToken();
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...authHeaders(sent), ...(init?.headers ?? {}) },
  });
  if (res.status === 401) reportUnauthorized(sent);
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
 * Subscribes to an SSE endpoint. Reconnects with Last-Event-ID after a 1 s pause, or at
 * once when the API token changes so the stream never outlives the token it was opened with.
 * Returns an unsubscribe function.
 */
export function subscribe(
  path: string,
  onEvent: (e: SSEEvent) => void,
  onStatus?: (s: "open" | "closed") => void,
  options?: SubscribeOptions,
) {
  let controller = new AbortController();
  let lastId: string | undefined = options?.lastEventId;
  let stopped = false;
  let sent = getToken();
  let tokenChanged = false;
  let wake: (() => void) | undefined;

  const offAuth = subscribeAuth(() => {
    if (getToken() === sent) return;
    tokenChanged = true;
    controller.abort();
    wake?.();
  });

  const pause = () =>
    new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, 1000);
      wake = () => {
        clearTimeout(timer);
        resolve();
      };
    });

  const connect = async () => {
    while (!stopped) {
      controller = new AbortController();
      sent = getToken();
      tokenChanged = false;
      try {
        const res = await fetch(path, {
          headers: { Accept: "text/event-stream", ...authHeaders(sent), ...(lastId ? { "Last-Event-ID": lastId } : {}) },
          signal: controller.signal,
        });
        if (res.status === 401) reportUnauthorized(sent);
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
      if (!tokenChanged) await pause();
    }
  };
  void connect();
  return () => {
    stopped = true;
    offAuth();
    controller.abort();
    wake?.();
  };
}

// ---------------------------------------------------------------------------------------
// Types derived from api/openapi.yaml (version 0.5.0) and the endpoint functions that use
// them. Optional fields are the ones the API may omit.
// ---------------------------------------------------------------------------------------

export type Status = {
  version: string;
  uptime_s: number;
  db_ok: boolean;
  workers: number;
  sse_clients: number;
  runs: Record<string, number>;
  /** Present once the scheduler runs. */
  scheduler?: { tick_seconds?: number; next_due_at?: string | null };
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

/** Runs of an agent that are in flight right now; the list endpoint fills it. */
export type AgentActivity = { running: number; waiting_approval: number };

export type Agent = AgentInput & {
  id: string;
  openclaw_agent_id?: string;
  materialized_hash?: string;
  activity?: AgentActivity;
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
  /** True for the tools an agent can also call while it talks to someone in a conversation. */
  chat?: boolean;
};

export const RUN_STATUSES = ["queued", "running", "waiting_approval", "succeeded", "failed", "cancelled", "timed_out"] as const;
export type RunStatus = (typeof RUN_STATUSES)[number];
export type RunTrigger = "manual" | "schedule" | "channel" | "eval";

const CONVERSATION_REF = "conversation:";

/**
 * The id of the conversation a run was delegated from: a run started from a conversation has
 * trigger "channel" and trigger_ref "conversation:<id>". Undefined for any other run, and
 * for a delegated run whose caller had no conversation (an empty ref).
 */
export function delegatedConversationId(run: Pick<Run, "trigger" | "trigger_ref">): string | undefined {
  if (run.trigger !== "channel") return undefined;
  const ref = run.trigger_ref?.trim() ?? "";
  if (!ref.startsWith(CONVERSATION_REF)) return undefined;
  return ref.slice(CONVERSATION_REF.length).trim() || undefined;
}

/** A run started from a conversation: trigger "channel" with a conversation reference, or with none. Any other reference is shown as it is. */
export const isDelegatedRun = (run: Pick<Run, "trigger" | "trigger_ref">): boolean =>
  run.trigger === "channel" && (!!delegatedConversationId(run) || !run.trigger_ref?.trim());

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
  /** When the outcome of a delegated run was reported into its conversation. */
  reported_at?: string | null;
  /** What the project checkout's manifest said when it was cloned; absent without a checkout. */
  toolchain?: Toolchain;
  /** The project the run works on; absent for a run without one. */
  project_id?: string;
  /** The project's settings as they were when the run was queued; never the token. */
  project?: ProjectSettings;
  /** How many times the operator retried the run from the step it stopped at (DESIGN 5.21). */
  retries?: number;
  /** The merge or pull request the run opened; absent when it opened none (DESIGN 5.20). */
  merge_request?: MergeRequest;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  updated_at?: string;
};

export type Toolchain = "go" | "node" | "python" | "default";

export type RunCreate = {
  workflow_id?: string;
  input?: Record<string, unknown>;
  agent_id?: string;
  task?: string;
  limits?: Limits;
  /** The project the run works on: required for a workflow that opts in, refused for one that does not, optional for a quick run. */
  project_id?: string;
};

// ---------------------------------------------------------------------------------------
// Projects (DESIGN 5.17): the git repositories runs work on. The token is write-only: the
// API says whether one is stored (`token_set`) and never returns it.
// ---------------------------------------------------------------------------------------

export type ProjectProvider = "gitlab" | "github";

/** A project's settings without its token; also a run's snapshot of the project it works on. */
export type ProjectSettings = {
  name: string;
  repo_url: string;
  provider: ProjectProvider;
  default_branch: string;
  sandbox_network: boolean;
};

/** The newest run that names the project. */
export type ProjectLastRun = {
  id: string;
  status: RunStatus;
  toolchain?: Toolchain;
  created_at: string;
  finished_at?: string;
};

/** What a project's runs spent in the current UTC day and month, in USD, whatever their status (DESIGN 5.19). */
export type ProjectSpend = {
  day_usd: number;
  month_usd: number;
  /** When the day window ends. */
  day_until?: string;
  /** When the month window ends. */
  month_until?: string;
};

export type Project = ProjectSettings & {
  id: string;
  description: string;
  /** A git token is stored; the token itself is never returned. */
  token_set: boolean;
  created_at: string;
  updated_at: string;
  last_run?: ProjectLastRun;
  /** The operator's standing instructions for every run on the project (DESIGN 5.18); empty when none. */
  instructions?: string;
  /** The UTC-day cap in USD; absent when there is none (DESIGN 5.19). */
  max_cost_usd_per_day?: number;
  /** The UTC-month cap in USD; absent when there is none. */
  max_cost_usd_per_month?: number;
  spend?: ProjectSpend;
};

export type ProjectInput = {
  name: string;
  repo_url: string;
  /** Empty: github for github.com, gitlab for any other host. */
  provider?: ProjectProvider | "";
  default_branch?: string;
  sandbox_network?: boolean;
  description?: string;
  /** Required on create; on update, absent or empty keeps the stored token. */
  token?: string;
  /** At most 8192 bytes; an update without it clears the instructions. */
  instructions?: string;
  /** The UTC-day cap in USD; null or absent clears it. */
  max_cost_usd_per_day?: number | null;
  /** The UTC-month cap in USD; null or absent clears it. */
  max_cost_usd_per_month?: number | null;
};

/** A note an agent saved for the project; any agent of a run on the project reads and writes them (DESIGN 5.18). */
export type ProjectFact = {
  id: string;
  project_id: string;
  key: string;
  value: string;
  /** The agent that saved it; absent once that agent is deleted. */
  agent_id?: string | null;
  agent_name?: string | null;
  source_run_id?: string | null;
  created_at: string;
  updated_at: string;
  expires_at?: string | null;
};

export type MergeRequestState = "open" | "merged" | "closed";
export type CIStatus = "none" | "pending" | "passed" | "failed";

/** A merge request (GitLab) or pull request (GitHub) the platform opened for a run; one per run (DESIGN 5.20). */
export type MergeRequest = {
  id: string;
  project_id: string;
  run_id: string;
  provider: ProjectProvider;
  /** The GitLab iid or the GitHub pull number. */
  number: number;
  url: string;
  source_branch: string;
  target_branch: string;
  title: string;
  state: MergeRequestState;
  ci_status: CIStatus;
  head_sha?: string;
  /** Null when the provider could not tell. */
  unresolved_threads?: number | null;
  last_sync_error?: string;
  synced_at?: string | null;
  reported_state?: "merged" | "closed";
  merged_by?: string;
  created_at: string;
  updated_at: string;
  /** When it was merged or closed. */
  closed_at?: string;
};

/** What a check of the stored token found; provider refusals come back as `ok: false`, not as errors. */
export type ProjectCheck = {
  ok: boolean;
  message: string;
  default_branch?: string;
  branch_exists?: boolean;
  can_push?: boolean;
  checked_at: string;
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
  /** Empty ("") for a call an agent made in a conversation, which is not part of a run. */
  run_id: string;
  run_node_id: string;
  /** Set for a call made in a conversation. */
  conversation_id?: string | null;
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
  kind: "run" | "node" | "model_call" | "tool_call" | "approval_wait" | "project";
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
  /** Empty for an approval raised in a conversation, which is not part of a run. */
  run_id: string | null;
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
  /** When the request was reported into the conversation of a delegated run. */
  reported_at?: string | null;
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
  /** Each run of the workflow names a project, which is cloned into the first agent's run directory (DESIGN 5.17). */
  project?: boolean;
  /** Where the builder drew each node; the backend keeps it with the definition. */
  ui?: { positions?: Record<string, Position> };
};

export type WorkflowInput = { name: string; description: string; definition: WorkflowDefinition };

export type Workflow = WorkflowInput & {
  id: string;
  version: number;
  template_key?: string;
  /** The template this workflow was made from. */
  from_template?: string;
  created_at: string;
  updated_at: string;
};

export type WorkflowTemplate = { key: string; name: string; description: string; definition: WorkflowDefinition };

/** One entry of a 422 `details` list. */
export type ValidationDetail = { path: string; message: string };

/** An approval as the inbox lists it: the tool call is joined for tool approvals, and the status of its run says whether that run has ended. */
export type ApprovalItem = Approval & { tool_call?: ToolCall; run_status?: RunStatus };

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

/** The newest runs first; `project_id` keeps the runs of one project (the filter is the server's, so `limit` counts that project's runs). */
export async function listRuns(opts: { status?: RunStatus; project_id?: string; limit?: number } = {}) {
  const q = new URLSearchParams();
  if (opts.status) q.set("status", opts.status);
  if (opts.project_id) q.set("project_id", opts.project_id);
  q.set("limit", String(opts.limit ?? 100));
  return (await api<Run[] | null>(`/api/runs?${q}`)) ?? [];
}
export const createRun = (body: RunCreate) => api<Run>("/api/runs", withBody("POST", body));
export const cancelRun = (id: string) => api<Run>(`/api/runs/${seg(id)}/cancel`, withBody("POST"));
/**
 * Re-queues a failed, timed-out or cancelled run from the step it stopped at (DESIGN 5.21). `limits`, when given,
 * replace the run's limits first, which is how a run that used up a cap is retried. A 409 carries `run_not_retryable`,
 * `retry_limit` or `limits_exhausted` (its details name each exhausted limit).
 */
export const retryRun = (id: string, limits?: Limits) => api<Run>(`/api/runs/${seg(id)}/retry`, withBody("POST", limits ? { limits } : undefined));
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

// ---------------------------------------------------------------------------------------
// Channels and conversations (DESIGN 5.8), schedules (5.9), skills and agent memory (5.10).
// ---------------------------------------------------------------------------------------

export type TelegramBinding = { agent_id: string; agent_name: string };

export type TelegramChannel = {
  enabled: boolean;
  /** polling, webhook or off. */
  mode: string;
  bot_username?: string;
  allowed_user_ids: number[];
  binding: TelegramBinding | null;
};

export type ChannelsInfo = { telegram: TelegramChannel };

export type Conversation = {
  id: string;
  channel: string;
  external_chat_id: string;
  /** The person's name as the channel reports it, when it does. */
  display_name?: string;
  agent_id: string;
  agent_name: string;
  last_message_at: string;
  message_count: number;
};

export type ConversationMessage = {
  id: string;
  direction: "in" | "out";
  text: string;
  /** Set on a platform report: the run whose outcome or approval the message announces. */
  run_id?: string | null;
  created_at: string;
};

export type ScheduleKind = "cron" | "delay";

export type ScheduleInput = {
  workflow_id: string;
  /** The project the schedule's runs work on: required for a workflow that opts in, refused for one that does not. */
  project_id?: string;
  kind: ScheduleKind;
  cron_expr?: string;
  tz: string;
  run_at?: string;
  enabled: boolean;
  input: Record<string, unknown>;
};

export type Schedule = ScheduleInput & {
  id: string;
  /** The workflow's name as the API joined it; it survives the workflow's deletion. */
  workflow_name?: string;
  /** The project's name as the API joined it; it survives the project's deletion. */
  project_name?: string;
  next_run_at?: string | null;
  last_run_at?: string | null;
  last_run_id?: string | null;
  last_error?: string | null;
  created_at: string;
  updated_at: string;
};

export type SkillInput = { name: string; description: string; body: string };
export type Skill = SkillInput & { id: string; created_at: string; updated_at: string };

export type MemoryFact = { id: string; key: string; value: string; created_at: string; expires_at?: string | null };

export const getChannels = async (): Promise<ChannelsInfo> => {
  const info = await api<ChannelsInfo>("/api/channels");
  return normalizeChannels(info);
};

export function normalizeChannels(info: ChannelsInfo): ChannelsInfo {
  const t = info?.telegram;
  return { ...info, telegram: { ...t, allowed_user_ids: t?.allowed_user_ids ?? [], binding: t?.binding ?? null } };
}

export const bindTelegram = async (agentId: string) =>
  normalizeChannels(await api<ChannelsInfo>("/api/channels/telegram/binding", withBody("PUT", { agent_id: agentId })));
export const unbindTelegram = () => api<ChannelsInfo | undefined>("/api/channels/telegram/binding", withBody("DELETE"));

export const listConversations = async () => (await api<Conversation[] | null>("/api/conversations")) ?? [];

export const listConversationMessages = async (id: string) =>
  (await api<ConversationMessage[] | null>(`/api/conversations/${seg(id)}/messages`)) ?? [];

/** The tool calls an agent made in a conversation, oldest first. */
export const listConversationToolCalls = async (id: string) =>
  (await api<ToolCall[] | null>(`/api/conversations/${seg(id)}/tool-calls`)) ?? [];

// Go encodes a nil map as null; the form treats the input as always present.
const normalizeSchedule = (s: Schedule): Schedule => ({ ...s, input: s.input ?? {} });

export const listSchedules = async () => ((await api<Schedule[] | null>("/api/schedules")) ?? []).map(normalizeSchedule);
export const createSchedule = async (input: ScheduleInput) => normalizeSchedule(await api<Schedule>("/api/schedules", withBody("POST", input)));
export const updateSchedule = async (id: string, input: ScheduleInput) =>
  normalizeSchedule(await api<Schedule>(`/api/schedules/${seg(id)}`, withBody("PUT", input)));
export const deleteSchedule = (id: string) => api<void>(`/api/schedules/${seg(id)}`, withBody("DELETE"));

export const listSkills = async () => (await api<Skill[] | null>("/api/skills")) ?? [];
export const createSkill = (input: SkillInput) => api<Skill>("/api/skills", withBody("POST", input));
export const updateSkill = (id: string, input: SkillInput) => api<Skill>(`/api/skills/${seg(id)}`, withBody("PUT", input));
export const deleteSkill = (id: string) => api<void>(`/api/skills/${seg(id)}`, withBody("DELETE"));

export const listAgentMemory = async (agentId: string) => (await api<MemoryFact[] | null>(`/api/agents/${seg(agentId)}/memory`)) ?? [];
export const deleteAgentMemory = (agentId: string, factId: string) =>
  api<void>(`/api/agents/${seg(agentId)}/memory/${seg(factId)}`, withBody("DELETE"));

export const listProjects = async () => (await api<Project[] | null>("/api/projects")) ?? [];
export const getProject = (id: string) => api<Project>(`/api/projects/${seg(id)}`);
export const createProject = (input: ProjectInput) => api<Project>("/api/projects", withBody("POST", input));
export const updateProject = (id: string, input: ProjectInput) => api<Project>(`/api/projects/${seg(id)}`, withBody("PUT", input));
export const deleteProject = (id: string) => api<void>(`/api/projects/${seg(id)}`, withBody("DELETE"));
export const checkProject = (id: string) => api<ProjectCheck>(`/api/projects/${seg(id)}/check`, withBody("POST"));

/** The project's live notes, newest first. */
export const listProjectFacts = async (projectId: string) => (await api<ProjectFact[] | null>(`/api/projects/${seg(projectId)}/facts`)) ?? [];
export const deleteProjectFact = (projectId: string, factId: string) =>
  api<void>(`/api/projects/${seg(projectId)}/facts/${seg(factId)}`, withBody("DELETE"));

// ---------------------------------------------------------------------------------------
// Merge requests the platform opened (DESIGN 5.20).
// ---------------------------------------------------------------------------------------

/** Newest first; the API answers 50 by default and 200 at most. */
export async function listMergeRequests(opts: { project_id?: string; run_id?: string; state?: MergeRequestState; limit?: number } = {}) {
  const q = new URLSearchParams();
  if (opts.project_id) q.set("project_id", opts.project_id);
  if (opts.run_id) q.set("run_id", opts.run_id);
  if (opts.state) q.set("state", opts.state);
  if (opts.limit) q.set("limit", String(opts.limit));
  const qs = q.toString();
  return (await api<MergeRequest[] | null>(`/api/merge-requests${qs ? `?${qs}` : ""}`)) ?? [];
}
/** Refreshes the record from the provider now; provider errors come back on the record as `last_sync_error`. */
export const syncMergeRequest = (id: string) => api<MergeRequest>(`/api/merge-requests/${seg(id)}/sync`, withBody("POST"));
/** Merges the request on the provider; a 409 carries the reason (`merge_request_not_open`, `merge_request_ci_not_passed`, `merge_request_not_mergeable`). */
export const mergeMergeRequest = (id: string) => api<MergeRequest>(`/api/merge-requests/${seg(id)}/merge`, withBody("POST"));
