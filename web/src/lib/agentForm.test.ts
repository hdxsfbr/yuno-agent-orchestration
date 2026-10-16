import { describe, expect, it } from "vitest";
import type { Agent } from "./api";
import {
  LIMIT_FIELDS,
  agentToForm,
  defaultPolicy,
  emptyForm,
  errorsFor,
  parseFieldErrors,
  pruneBlankPatterns,
  toInput,
  unplacedErrors,
  validateForm,
} from "./agentForm";

const agent: Agent = {
  id: "a-1",
  name: "Release helper",
  role: "Ships releases",
  system_prompt: "Be careful.",
  model: "claude-sonnet-5-5",
  effort: "high",
  thinking: "adaptive",
  max_tokens: 4096,
  tool_allowlist: ["read", "deploy_release"],
  interaction_rules: { deploy_release: "require_approval", read: "auto" },
  limits: { max_steps: 25, max_cost_usd: 1.5 },
  guardrails: {
    max_cost_usd_per_run: 0.75,
    max_runs_per_hour: 6,
    blocked_tools: ["gitlab_create_mr"],
    blocked_input_patterns: [{ name: "no-rm", tool: "exec", regex: "rm\\s+-rf" }],
  },
  memory_policy: { enabled: true, max_facts: 50, fact_ttl_days: 30 },
  exec_policy: { security: "allowlist", ask: "always", ask_fallback: "deny", allowlist: ["git status"] },
  skills: ["code-review"],
  channels: ["telegram"],
  openclaw_agent_id: "release-helper",
  created_at: "2026-10-15T10:00:00Z",
  updated_at: "2026-10-15T10:00:00Z",
};

describe("agentToForm and toInput", () => {
  it("round-trips an agent without changing its configuration", () => {
    const input = toInput(agentToForm(agent));
    expect(input).toEqual({
      name: "Release helper",
      role: "Ships releases",
      system_prompt: "Be careful.",
      model: "claude-sonnet-5-5",
      thinking: "adaptive",
      max_tokens: 4096,
      tool_allowlist: ["deploy_release", "read"],
      interaction_rules: { deploy_release: "require_approval", read: "auto" },
      limits: { max_steps: 25, max_cost_usd: 1.5 },
      guardrails: {
        max_cost_usd_per_run: 0.75,
        max_runs_per_hour: 6,
        blocked_tools: ["gitlab_create_mr"],
        blocked_input_patterns: [{ name: "no-rm", tool: "exec", regex: "rm\\s+-rf" }],
      },
      memory_policy: { enabled: true, max_facts: 50, fact_ttl_days: 30 },
      exec_policy: { security: "allowlist", ask: "always", ask_fallback: "deny", allowlist: ["git status"] },
      skills: ["code-review"],
      channels: ["telegram"],
    });
  });

  it("builds a minimal valid input from an empty form", () => {
    const f = { ...emptyForm(), name: "  Scout  " };
    expect(validateForm(f)).toEqual({});
    const input = toInput(f);
    expect(input).toMatchObject({ name: "Scout", model: "claude-opus-5-5", thinking: "medium", tool_allowlist: [], interaction_rules: {}, limits: {}, channels: [] });
    // effort has no OpenClaw counterpart: the form neither shows nor sends it
    expect(input).not.toHaveProperty("effort");
    expect(input.max_tokens).toBeUndefined();
    expect(input.exec_policy).toEqual({ security: "allowlist", ask: "on-miss", ask_fallback: "deny", allowlist: [] });
  });

  it("writes a rule only for allowed tools whose policy was set", () => {
    const f = { ...emptyForm(), name: "x", tools: { read: { policy: null }, exec: { policy: "blocked" as const } } };
    expect(toInput(f)).toMatchObject({ tool_allowlist: ["exec", "read"], interaction_rules: { exec: "blocked" } });
  });

  it("treats empty and zero limits as unset", () => {
    const f = { ...emptyForm(), name: "x" };
    f.limits = { ...f.limits, max_steps: "0", max_tool_calls: "" };
    expect(toInput(f).limits).toEqual({});
  });

  it("has every limit with its platform default", () => {
    expect(Object.fromEntries(LIMIT_FIELDS.map((l) => [l.key, l.fallback]))).toEqual({
      max_steps: "40",
      max_duration_seconds: "600",
      max_node_iterations: "12",
      max_loop_visits: "3",
      max_cost_usd: "2.00",
      max_tool_calls: "60",
      approval_timeout_seconds: "86400",
    });
  });
});

describe("validateForm", () => {
  it("requires a name and a model", () => {
    const e = validateForm({ ...emptyForm(), name: "   ", model: "" });
    expect(e.name).toEqual(["Name is required"]);
    expect(e.model).toEqual(["Model is required"]);
  });

  it("limits the name to 80 characters", () => {
    expect(validateForm({ ...emptyForm(), name: "x".repeat(81) }).name).toEqual(["Name must be 80 characters or fewer"]);
    expect(validateForm({ ...emptyForm(), name: "x".repeat(80) }).name).toBeUndefined();
  });

  it("checks numbers", () => {
    const f = { ...emptyForm(), name: "x", maxTokens: "12.5", maxCostPerRun: "abc", maxRunsPerHour: "-1" };
    f.limits = { ...f.limits, max_steps: "ten", max_cost_usd: "2.5" };
    const e = validateForm(f);
    expect(e["max_tokens"]).toBeDefined();
    expect(e["guardrails.max_cost_usd_per_run"]).toBeDefined();
    expect(e["guardrails.max_runs_per_hour"]).toBeDefined();
    expect(e["limits.max_steps"]).toBeDefined();
    expect(e["limits.max_cost_usd"]).toBeUndefined();
  });

  it("requires all three parts of a started pattern row and ignores blank rows", () => {
    const f = { ...emptyForm(), name: "x", patterns: [{ name: "a", tool: "", regex: "" }, { name: "", tool: "", regex: "" }] };
    const e = validateForm(f);
    expect(e["guardrails.blocked_input_patterns.0.tool"]).toEqual(["Tool is required"]);
    expect(e["guardrails.blocked_input_patterns.0.regex"]).toEqual(["Pattern is required"]);
    expect(Object.keys(e).some((k) => k.includes(".1."))).toBe(false);
    expect(pruneBlankPatterns(f).patterns).toHaveLength(1);
  });

  it("checks skill names", () => {
    expect(validateForm({ ...emptyForm(), name: "x", skills: ["code-review", "Bad Name"] })["skills.1"]).toBeDefined();
  });
});

describe("defaultPolicy", () => {
  it("requires approval for side-effecting tools only (DESIGN 5.3 rule 6)", () => {
    expect(defaultPolicy({ risk: "side_effecting" })).toBe("require_approval");
    expect(defaultPolicy({ risk: "read_only" })).toBe("auto");
    expect(defaultPolicy({ risk: "computational" })).toBe("auto");
    expect(defaultPolicy(undefined)).toBe("auto");
  });
});

describe("parseFieldErrors", () => {
  it("reads a list of path: message strings", () => {
    const e = parseFieldErrors(["name: is required", "limits.max_steps: must be at least 1", "guardrails.blocked_input_patterns[0].regex: invalid regular expression"]);
    expect(e["name"]).toEqual(["is required"]);
    expect(e["limits.max_steps"]).toEqual(["must be at least 1"]);
    expect(e["guardrails.blocked_input_patterns.0.regex"]).toEqual(["invalid regular expression"]);
  });

  it("reads an object keyed by field, with strings, lists or nested objects", () => {
    const e = parseFieldErrors({ name: "too long", skills: ["bad name"], limits: { max_cost_usd: "negative" } });
    expect(e["name"]).toEqual(["too long"]);
    expect(e["skills"]).toEqual(["bad name"]);
    expect(e["limits.max_cost_usd"]).toEqual(["negative"]);
  });

  it("reads the details the API sends for a 422: a list of {path, message}", () => {
    const e = parseFieldErrors([
      { path: "name", message: "is required" },
      { path: "tool_allowlist[2]", message: 'unknown tool "nope"' },
      { path: "interaction_rules.exec", message: "must be one of auto, require_approval, blocked" },
      { path: "", message: "set workflow_id or agent_id" },
    ]);
    expect(e["name"]).toEqual(["is required"]);
    expect(errorsFor(e, "tool_allowlist")).toEqual(['unknown tool "nope"']);
    expect(e["interaction_rules.exec"]).toHaveLength(1);
    expect(unplacedErrors(e)).toEqual(["set workflow_id or agent_id"]);
  });

  it("reads wrapped lists of field/message objects", () => {
    const e = parseFieldErrors({ fields: [{ field: "model", message: "unknown model" }, { path: "exec_policy.security", msg: "must be one of deny, allowlist, full" }] });
    expect(e["model"]).toEqual(["unknown model"]);
    expect(e["exec_policy.security"]).toEqual(["must be one of deny, allowlist, full"]);
  });

  it("keeps messages without a path as form-level errors", () => {
    const e = parseFieldErrors("something is wrong");
    expect(e[""]).toEqual(["something is wrong"]);
    expect(unplacedErrors(e)).toEqual(["something is wrong"]);
    expect(parseFieldErrors(undefined)).toEqual({});
  });

  it("finds errors for a field and everything below it", () => {
    const e = parseFieldErrors({ skills: ["a"], "skills.2": "b", "skills_other": "c" });
    expect(errorsFor(e, "skills")).toEqual(["a", "b"]);
  });

  it("reports fields the form does not have", () => {
    expect(unplacedErrors(parseFieldErrors(["colour: not supported"]))).toEqual(["colour: not supported"]);
    expect(unplacedErrors(parseFieldErrors(["name: bad"]))).toEqual([]);
  });
});

describe("memory policy", () => {
  it("never sends a conversation window, whether or not the saved agent has one", () => {
    const withWindow = { ...agent, memory_policy: { enabled: true, conversation_window: 30, conversation_ttl_days: 14 } as Agent["memory_policy"] };
    expect(emptyForm()).not.toHaveProperty("conversationWindow");
    expect(toInput(agentToForm(withWindow)).memory_policy).toEqual({ enabled: true, conversation_ttl_days: 14 });
    expect(validateForm(agentToForm(withWindow))).toEqual({});
  });
});
