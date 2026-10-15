import type {
  Agent,
  AgentInput,
  ExecAsk,
  ExecSecurity,
  Limits,
  Tool,
  ToolPolicy,
  ThinkingLevel,
} from "./api";

// Form state for the agent editor. Numbers are kept as the text the user typed so an empty
// field means "not set"; toInput() turns the state into the AgentInput the API takes.

export const DEFAULT_MODEL = "claude-opus-5-5";

/** Operating limits with the platform defaults of DESIGN 5.2, shown as placeholders. */
export const LIMIT_FIELDS = [
  { key: "max_steps", label: "Max steps", fallback: "40", integer: true },
  { key: "max_duration_seconds", label: "Max duration (seconds)", fallback: "600", integer: true },
  { key: "max_node_iterations", label: "Max node iterations", fallback: "12", integer: true },
  { key: "max_loop_visits", label: "Max loop visits", fallback: "3", integer: true },
  { key: "max_cost_usd", label: "Max cost (USD)", fallback: "2.00", integer: false },
  { key: "max_tool_calls", label: "Max tool calls", fallback: "60", integer: true },
  { key: "approval_timeout_seconds", label: "Approval timeout (seconds)", fallback: "86400", integer: true },
] as const satisfies readonly { key: keyof Limits; label: string; fallback: string; integer: boolean }[];

export type LimitKey = (typeof LIMIT_FIELDS)[number]["key"];

export type PatternRow = { name: string; tool: string; regex: string };

export type FormState = {
  name: string;
  role: string;
  systemPrompt: string;
  model: string;
  thinking: ThinkingLevel;
  maxTokens: string;
  /** Allowed tools by name. The policy is null until the user picks one explicitly. */
  tools: Record<string, { policy: ToolPolicy | null }>;
  limits: Record<LimitKey, string>;
  maxCostPerRun: string;
  maxRunsPerHour: string;
  blockedTools: string[];
  patterns: PatternRow[];
  memoryEnabled: boolean;
  maxFacts: string;
  factTtlDays: string;
  conversationWindow: string;
  conversationTtlDays: string;
  execSecurity: ExecSecurity;
  execAsk: ExecAsk;
  execAskFallback: ExecSecurity;
  execAllowlist: string[];
  skills: string[];
  telegram: boolean;
};

const emptyLimits = (): Record<LimitKey, string> =>
  Object.fromEntries(LIMIT_FIELDS.map((f) => [f.key, ""])) as Record<LimitKey, string>;

export function emptyForm(): FormState {
  return {
    name: "",
    role: "",
    systemPrompt: "",
    model: DEFAULT_MODEL,
    thinking: "medium",
    maxTokens: "",
    tools: {},
    limits: emptyLimits(),
    maxCostPerRun: "",
    maxRunsPerHour: "",
    blockedTools: [],
    patterns: [],
    memoryEnabled: false,
    maxFacts: "",
    factTtlDays: "",
    conversationWindow: "",
    conversationTtlDays: "",
    // The policy OpenClaw applies to elevated exec unless told otherwise (DESIGN 5.3 rule 1).
    execSecurity: "allowlist",
    execAsk: "on-miss",
    execAskFallback: "deny",
    execAllowlist: [],
    skills: [],
    telegram: false,
  };
}

const text = (n: number | undefined) => (n ? String(n) : "");

export function agentToForm(a: Agent): FormState {
  const base = emptyForm();
  const rules = a.interaction_rules ?? {};
  const limits = emptyLimits();
  for (const f of LIMIT_FIELDS) limits[f.key] = text(a.limits?.[f.key]);
  return {
    ...base,
    name: a.name,
    role: a.role ?? "",
    systemPrompt: a.system_prompt ?? "",
    model: a.model || DEFAULT_MODEL,
    thinking: a.thinking ?? base.thinking,
    maxTokens: text(a.max_tokens),
    tools: Object.fromEntries((a.tool_allowlist ?? []).map((n) => [n, { policy: rules[n] ?? null }])),
    limits,
    maxCostPerRun: text(a.guardrails?.max_cost_usd_per_run),
    maxRunsPerHour: text(a.guardrails?.max_runs_per_hour),
    blockedTools: a.guardrails?.blocked_tools ?? [],
    patterns: (a.guardrails?.blocked_input_patterns ?? []).map((p) => ({ name: p.name, tool: p.tool, regex: p.regex })),
    memoryEnabled: a.memory_policy?.enabled ?? false,
    maxFacts: text(a.memory_policy?.max_facts),
    factTtlDays: text(a.memory_policy?.fact_ttl_days),
    conversationWindow: text(a.memory_policy?.conversation_window),
    conversationTtlDays: text(a.memory_policy?.conversation_ttl_days),
    execSecurity: a.exec_policy?.security ?? base.execSecurity,
    execAsk: a.exec_policy?.ask ?? base.execAsk,
    execAskFallback: a.exec_policy?.ask_fallback ?? base.execAskFallback,
    execAllowlist: a.exec_policy?.allowlist ?? [],
    skills: a.skills ?? [],
    telegram: (a.channels ?? []).includes("telegram"),
  };
}

/** DESIGN 5.3 rule 6: without a rule a side-effecting tool needs approval, others run. */
export function defaultPolicy(tool: Pick<Tool, "risk"> | undefined): ToolPolicy {
  return tool?.risk === "side_effecting" ? "require_approval" : "auto";
}

export const SKILL_NAME = /^[a-z0-9]+(-[a-z0-9]+)*$/;

// ---------------------------------------------------------------------------------------
// Field errors, keyed by dotted path ("limits.max_steps"). "" holds errors with no field.
// ---------------------------------------------------------------------------------------

export type FieldErrors = Record<string, string[]>;

export function addError(errors: FieldErrors, path: string, message: string): void {
  (errors[path] ??= []).push(message);
}

/** Messages for `path` itself and everything below it ("skills" includes "skills.0"). */
export function errorsFor(errors: FieldErrors, path: string): string[] {
  const out: string[] = [];
  for (const [key, messages] of Object.entries(errors)) {
    if (key === path || key.startsWith(`${path}.`)) out.push(...messages);
  }
  return out;
}

/** Top-level members of AgentInput; errors elsewhere cannot be shown next to a field. */
const KNOWN_ROOTS = new Set([
  "name",
  "role",
  "system_prompt",
  "model",
  "thinking",
  "max_tokens",
  "tool_allowlist",
  "interaction_rules",
  "limits",
  "guardrails",
  "memory_policy",
  "exec_policy",
  "skills",
  "channels",
]);

/** Errors whose path does not belong to a field of the form. */
export function unplacedErrors(errors: FieldErrors): string[] {
  const out: string[] = [];
  for (const [path, messages] of Object.entries(errors)) {
    if (!KNOWN_ROOTS.has(path.split(".")[0])) out.push(...messages.map((m) => (path ? `${path}: ${m}` : m)));
  }
  return out;
}

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

function normalizePath(path: string): string {
  return path
    .replace(/\[(\d+)\]/g, ".$1")
    .replace(/^\.+/, "")
    .replace(/^(agent|body|input)\./, "");
}

/**
 * Reads the `details` of a 422 into field errors. The API documents `details` as free
 * form, so this accepts the shapes a validator is likely to produce: a list of
 * "path: message" strings, a list of {field, message} objects, or an object keyed by
 * field (optionally wrapped in `fields`, `errors` or `messages`).
 */
export function parseFieldErrors(details: unknown): FieldErrors {
  const out: FieldErrors = {};
  const visit = (value: unknown, path: string): void => {
    if (value == null) return;
    if (typeof value === "string") {
      const m = /^([A-Za-z_][\w.[\]-]*):\s+(.+)$/s.exec(value);
      if (path === "" && m) addError(out, normalizePath(m[1]), m[2]);
      else addError(out, normalizePath(path), value);
      return;
    }
    if (Array.isArray(value)) {
      value.forEach((v, i) => {
        if (isRecord(v) && (typeof v.message === "string" || typeof v.msg === "string")) {
          const p = [v.field, v.path, v.name, v.loc].find((x): x is string => typeof x === "string");
          addError(out, normalizePath(p ?? path), String(v.message ?? v.msg));
        } else if (typeof v === "string") visit(v, path);
        else visit(v, path ? `${path}.${i}` : String(i));
      });
      return;
    }
    if (isRecord(value)) {
      for (const [k, v] of Object.entries(value)) {
        const wrapper = path === "" && (k === "fields" || k === "errors" || k === "messages" || k === "violations");
        visit(v, wrapper ? path : path ? `${path}.${k}` : k);
      }
    }
  };
  visit(details, "");
  return out;
}

// ---------------------------------------------------------------------------------------
// Validation and conversion
// ---------------------------------------------------------------------------------------

const INTEGER = /^\d+$/;
const DECIMAL = /^\d+(\.\d+)?$/;

function checkNumber(errors: FieldErrors, path: string, value: string, label: string, integer: boolean): void {
  const v = value.trim();
  if (v === "") return;
  if (!(integer ? INTEGER : DECIMAL).test(v)) {
    addError(errors, path, integer ? `${label} must be a whole number` : `${label} must be a number`);
  }
}

/** Drops pattern rows the user left completely empty, so row numbers match the request. */
export function pruneBlankPatterns(f: FormState): FormState {
  const patterns = f.patterns.filter((p) => p.name.trim() || p.tool.trim() || p.regex.trim());
  return patterns.length === f.patterns.length ? f : { ...f, patterns };
}

export function validateForm(f: FormState): FieldErrors {
  const errors: FieldErrors = {};
  const name = f.name.trim();
  if (name === "") addError(errors, "name", "Name is required");
  else if (name.length > 80) addError(errors, "name", "Name must be 80 characters or fewer");
  if (f.model.trim() === "") addError(errors, "model", "Model is required");

  checkNumber(errors, "max_tokens", f.maxTokens, "Max tokens", true);
  for (const l of LIMIT_FIELDS) checkNumber(errors, `limits.${l.key}`, f.limits[l.key], l.label, l.integer);
  checkNumber(errors, "guardrails.max_cost_usd_per_run", f.maxCostPerRun, "Cost per run", false);
  checkNumber(errors, "guardrails.max_runs_per_hour", f.maxRunsPerHour, "Runs per hour", true);
  checkNumber(errors, "memory_policy.max_facts", f.maxFacts, "Max facts", true);
  checkNumber(errors, "memory_policy.fact_ttl_days", f.factTtlDays, "Fact lifetime", true);
  checkNumber(errors, "memory_policy.conversation_window", f.conversationWindow, "Conversation window", true);
  checkNumber(errors, "memory_policy.conversation_ttl_days", f.conversationTtlDays, "Conversation lifetime", true);

  f.patterns.forEach((p, i) => {
    if (!p.name.trim() && !p.tool.trim() && !p.regex.trim()) return; // left empty: dropped on submit
    const base = `guardrails.blocked_input_patterns.${i}`;
    if (p.name.trim() === "") addError(errors, `${base}.name`, "Name is required");
    if (p.tool.trim() === "") addError(errors, `${base}.tool`, "Tool is required");
    if (p.regex.trim() === "") addError(errors, `${base}.regex`, "Pattern is required");
  });

  f.skills.forEach((s, i) => {
    if (!SKILL_NAME.test(s)) addError(errors, `skills.${i}`, `"${s}" must be lowercase words separated by hyphens`);
  });
  return errors;
}

const num = (s: string): number | undefined => {
  const v = s.trim();
  if (v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) && n !== 0 ? n : undefined;
};

/** The request body for the form. Call validateForm first; this assumes valid numbers. */
export function toInput(f: FormState): AgentInput {
  const limits: Limits = {};
  for (const l of LIMIT_FIELDS) {
    const v = num(f.limits[l.key]);
    if (v !== undefined) limits[l.key] = v;
  }

  const allowed = Object.keys(f.tools).sort();
  const rules: Record<string, ToolPolicy> = {};
  for (const n of allowed) {
    const p = f.tools[n].policy;
    if (p) rules[n] = p;
  }

  const input: AgentInput = {
    name: f.name.trim(),
    role: f.role.trim(),
    system_prompt: f.systemPrompt,
    model: f.model.trim(),
    thinking: f.thinking,
    tool_allowlist: allowed,
    interaction_rules: rules,
    limits,
    guardrails: {
      blocked_tools: f.blockedTools,
      blocked_input_patterns: f.patterns.map((p) => ({ name: p.name.trim(), tool: p.tool.trim(), regex: p.regex })),
    },
    memory_policy: { enabled: f.memoryEnabled },
    exec_policy: {
      security: f.execSecurity,
      ask: f.execAsk,
      ask_fallback: f.execAskFallback,
      allowlist: f.execAllowlist,
    },
    skills: f.skills,
    channels: f.telegram ? ["telegram"] : [],
  };

  const maxTokens = num(f.maxTokens);
  if (maxTokens !== undefined) input.max_tokens = maxTokens;
  const g = input.guardrails!;
  const cost = num(f.maxCostPerRun);
  if (cost !== undefined) g.max_cost_usd_per_run = cost;
  const perHour = num(f.maxRunsPerHour);
  if (perHour !== undefined) g.max_runs_per_hour = perHour;
  const m = input.memory_policy!;
  const facts = num(f.maxFacts);
  if (facts !== undefined) m.max_facts = facts;
  const factTtl = num(f.factTtlDays);
  if (factTtl !== undefined) m.fact_ttl_days = factTtl;
  const window = num(f.conversationWindow);
  if (window !== undefined) m.conversation_window = window;
  const convTtl = num(f.conversationTtlDays);
  if (convTtl !== undefined) m.conversation_ttl_days = convTtl;
  return input;
}
