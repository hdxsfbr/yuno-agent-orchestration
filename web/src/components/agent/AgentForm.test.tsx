// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import type { Agent, AgentInput, Skill, Tool } from "@/lib/api";
import type { FieldErrors } from "@/lib/agentForm";
import AgentForm from "./AgentForm";

afterEach(cleanup);

const tools: Tool[] = [
  { name: "read", description: "Read a file", risk: "read_only", hits: "none", timeout_seconds: 10, source: "openclaw" },
  { name: "deploy_release", description: "Deploy a release", risk: "side_effecting", hits: "external", timeout_seconds: 60, source: "platform" },
  { name: "memory_recall", description: "Recall facts", risk: "read_only", hits: "postgres", timeout_seconds: 10, source: "platform" },
];

const skills: Skill[] = [
  { id: "s1", name: "code-review", description: "Reviews diffs for bugs", body: "# Review", created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T09:00:00Z" },
  { id: "s2", name: "release-notes", description: "", body: "# Notes", created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T09:00:00Z" },
];

const saved = (over: Partial<Agent>): Agent => ({
  id: "a1",
  name: "Scout",
  model: "claude-opus-5-5",
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T09:00:00Z",
  ...over,
});

function setup(over: { serverErrors?: FieldErrors; formError?: string; agent?: Agent; telegram?: ReactNode; memoryFacts?: ReactNode } = {}) {
  const onSubmit = vi.fn<(input: AgentInput) => void>();
  render(
    <MemoryRouter>
      <AgentForm
        agent={over.agent}
        tools={{ data: tools, isLoading: false, error: null, refetch: () => undefined }}
        skills={{ data: skills, isLoading: false, error: null, refetch: () => undefined }}
        saving={false}
        serverErrors={over.serverErrors ?? {}}
        formError={over.formError}
        submitLabel="Create agent"
        onSubmit={onSubmit}
        telegram={over.telegram}
        memoryFacts={over.memoryFacts}
      />
    </MemoryRouter>,
  );
  return { onSubmit };
}

const submit = () => fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

describe("AgentForm validation", () => {
  it("refuses an empty name and says so next to the field", () => {
    const { onSubmit } = setup();
    submit();
    expect(onSubmit).not.toHaveBeenCalled();
    const name = screen.getByLabelText(/^Name/);
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText("Name is required")).not.toBeNull();
    expect(screen.getByText("Some fields need attention.")).not.toBeNull();
  });

  it("clears the message once the name is fixed and submits the input", () => {
    const { onSubmit } = setup();
    submit();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    expect(screen.queryByText("Name is required")).toBeNull();
    submit();
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit.mock.calls[0][0]).toMatchObject({ name: "Scout", model: "claude-opus-5-5", thinking: "medium" });
    expect(onSubmit.mock.calls[0][0]).not.toHaveProperty("effort");
    expect(screen.queryByLabelText(/^Effort/)).toBeNull();
    expect(screen.getByLabelText(/^Thinking/)).not.toBeNull();
  });

  it("flags a limit that is not a number and does not submit", () => {
    const { onSubmit } = setup();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    fireEvent.change(screen.getByLabelText("Max steps"), { target: { value: "lots" } });
    submit();
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Max steps must be a whole number")).not.toBeNull();
  });

  it("shows every limit with its default as the placeholder", () => {
    setup();
    expect((screen.getByLabelText("Max steps") as HTMLInputElement).placeholder).toBe("40");
    expect((screen.getByLabelText("Max duration (seconds)") as HTMLInputElement).placeholder).toBe("600");
    expect((screen.getByLabelText("Max cost (USD)") as HTMLInputElement).placeholder).toBe("2.00");
    expect((screen.getByLabelText("Approval timeout (seconds)") as HTMLInputElement).placeholder).toBe("86400");
  });
});

describe("AgentForm server errors", () => {
  it("renders 422 details next to the fields they name", () => {
    setup({ serverErrors: { name: ["name already taken"], "limits.max_steps": ["must be at least 1"] }, formError: "validation failed" });
    const steps = screen.getByLabelText("Max steps");
    expect(steps.getAttribute("aria-invalid")).toBe("true");
    expect(steps.parentElement?.textContent).toContain("must be at least 1");
    expect(screen.getByLabelText(/^Name/).parentElement?.textContent).toContain("name already taken");
    expect(screen.getByText("validation failed")).not.toBeNull();
  });

  it("lists errors that belong to no field in the summary", () => {
    setup({ serverErrors: { colour: ["not supported"] } });
    expect(screen.getByText("colour: not supported")).not.toBeNull();
  });
});

describe("AgentForm tools", () => {
  it("groups tools by source and shows risk", () => {
    setup();
    expect(screen.getByText("Platform tools")).not.toBeNull();
    expect(screen.getByText("OpenClaw built-ins")).not.toBeNull();
    expect(screen.getByText("side effecting")).not.toBeNull();
  });

  it("offers a policy only for allowed tools and writes it to interaction_rules", () => {
    const { onSubmit } = setup();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Releaser" } });
    expect(screen.queryByLabelText("Policy for deploy_release")).toBeNull();

    fireEvent.click(screen.getByLabelText(/deploy_release/));
    const policy = screen.getByLabelText("Policy for deploy_release") as HTMLSelectElement;
    expect(policy.value).toBe("require_approval");

    fireEvent.change(policy, { target: { value: "auto" } });
    expect(screen.getByText(/will run without asking for approval/)).not.toBeNull();

    fireEvent.click(screen.getByLabelText(/^read/));
    submit();
    const input = onSubmit.mock.calls[0][0];
    expect(input.tool_allowlist).toEqual(["deploy_release", "read"]);
    expect(input.interaction_rules).toEqual({ deploy_release: "auto" });
  });
});

describe("AgentForm lists", () => {
  it("offers the skills that exist and sends the ones ticked", () => {
    const { onSubmit } = setup();
    const group = within(screen.getByRole("group", { name: "Skills" }));
    expect(group.getByText("Reviews diffs for bugs")).not.toBeNull();
    expect(group.getByRole("link", { name: "Skills page" }).getAttribute("href")).toBe("/skills");

    fireEvent.click(group.getByRole("checkbox", { name: /code-review/ }));
    fireEvent.click(group.getByRole("checkbox", { name: /release-notes/ }));
    fireEvent.click(group.getByRole("checkbox", { name: /release-notes/ }));
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    submit();
    expect(onSubmit.mock.calls[0][0].skills).toEqual(["code-review"]);
  });

  it("keeps a skill the agent lists although it was deleted, so it can be removed", () => {
    const { onSubmit } = setup({ agent: saved({ skills: ["code-review", "retired-skill"] }) });
    const group = within(screen.getByRole("group", { name: "Skills" }));
    expect((group.getByRole("checkbox", { name: /code-review/ }) as HTMLInputElement).checked).toBe(true);
    const gone = group.getByRole("checkbox", { name: /retired-skill/ }) as HTMLInputElement;
    expect(gone.checked).toBe(true);
    expect(group.getByText("This skill no longer exists.")).not.toBeNull();
    fireEvent.click(gone);
    submit();
    expect(onSubmit.mock.calls[0][0].skills).toEqual(["code-review"]);
  });

  it("says so when there are no skills yet", () => {
    render(
      <MemoryRouter>
        <AgentForm
          tools={{ data: tools, isLoading: false, error: null, refetch: () => undefined }}
          skills={{ data: [], isLoading: false, error: null, refetch: () => undefined }}
          saving={false}
          serverErrors={{}}
          submitLabel="Create agent"
          onSubmit={() => undefined}
        />
      </MemoryRouter>,
    );
    expect(within(screen.getByRole("group", { name: "Skills" })).getByText("No skills yet.")).not.toBeNull();
  });

  it("collects blocked input patterns as rows and drops empty ones", () => {
    const { onSubmit } = setup();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    fireEvent.click(screen.getByRole("button", { name: "Add pattern" }));
    fireEvent.click(screen.getByRole("button", { name: "Add pattern" }));
    fireEvent.change(screen.getByLabelText("Pattern 2 name"), { target: { value: "no-rm" } });
    fireEvent.change(screen.getByLabelText("Pattern 2 tool"), { target: { value: "exec" } });
    fireEvent.change(screen.getByLabelText("Pattern 2 regex"), { target: { value: "rm -rf" } });
    submit();
    expect(onSubmit.mock.calls[0][0].guardrails?.blocked_input_patterns).toEqual([{ name: "no-rm", tool: "exec", regex: "rm -rf" }]);
  });

  it("sets the exec policy and leaves Telegram to the Channels page", () => {
    const { onSubmit } = setup();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    expect(screen.queryByLabelText(/^Telegram/)).toBeNull();
    fireEvent.change(screen.getByLabelText("Security"), { target: { value: "full" } });
    const allow = screen.getByLabelText("Allowlist");
    fireEvent.change(allow, { target: { value: "git status" } });
    fireEvent.keyDown(allow, { key: "Enter" });
    submit();
    const input = onSubmit.mock.calls[0][0];
    expect(input.channels).toEqual([]);
    expect(input.exec_policy).toMatchObject({ security: "full", allowlist: ["git status"] });
  });

  it("keeps the channels an agent already has when it is saved", () => {
    const { onSubmit } = setup({ agent: saved({ channels: ["telegram"] }) });
    submit();
    expect(onSubmit.mock.calls[0][0].channels).toEqual(["telegram"]);
  });
});

describe("AgentForm slots", () => {
  it("shows the Telegram line with the skills and the saved facts with the memory settings", () => {
    setup({ telegram: <p>Telegram line</p>, memoryFacts: <p>Saved facts list</p> });
    const skillsSection = screen.getByRole("group", { name: "Skills" }).closest("div.space-y-4")!;
    expect(within(skillsSection as HTMLElement).getByText("Telegram line")).not.toBeNull();
    const memory = screen.getByText("Remember facts between runs").closest("section, div.rounded-lg")!;
    expect(within(memory as HTMLElement).getByText("Saved facts list")).not.toBeNull();
  });
});

describe("AgentForm guardrail errors", () => {
  it("puts a bad expression's 422 next to that pattern's expression field", () => {
    setup({
      agent: saved({ guardrails: { blocked_input_patterns: [{ name: "no-rm", tool: "exec", regex: "rm -rf" }, { name: "no-curl", tool: "exec", regex: "(" }] } }),
      serverErrors: { "guardrails.blocked_input_patterns.1.pattern": ["error parsing regexp: missing closing )"] },
      formError: "validation failed",
    });
    const bad = screen.getByLabelText("Pattern 2 regex");
    expect(bad.getAttribute("aria-invalid")).toBe("true");
    expect(bad.parentElement?.textContent).toContain("missing closing )");
    expect(screen.getByLabelText("Pattern 1 regex").getAttribute("aria-invalid")).toBeNull();
  });

  it("shows the limits' 422 next to cost and rate fields", () => {
    setup({ serverErrors: { "guardrails.max_cost_usd_per_run": ["must be positive"], "guardrails.max_runs_per_hour": ["must be a whole number"] } });
    expect(screen.getByLabelText("Cost per run (USD)").parentElement?.textContent).toContain("must be positive");
    expect(screen.getByLabelText("Runs per hour").parentElement?.textContent).toContain("must be a whole number");
  });
});

describe("AgentForm memory and limits", () => {
  it("has no conversation window field and sends none, even for an agent that saved one", () => {
    const legacy = saved({ memory_policy: { enabled: true, max_facts: 25, conversation_ttl_days: 14, conversation_window: 30 } as Agent["memory_policy"] });
    const { onSubmit } = setup({ agent: legacy });
    expect(screen.queryByLabelText(/conversation window/i)).toBeNull();
    expect(screen.queryByText(/conversation window/i)).toBeNull();
    // the other memory settings are still there, filled from the agent
    expect((screen.getByLabelText("Max facts") as HTMLInputElement).value).toBe("25");
    expect((screen.getByLabelText("Conversation lifetime (days)") as HTMLInputElement).value).toBe("14");

    submit();
    const policy = onSubmit.mock.calls[0][0].memory_policy!;
    expect(policy).not.toHaveProperty("conversation_window");
    expect(policy).toEqual({ enabled: true, max_facts: 25, conversation_ttl_days: 14 });
  });

  it("says which limits apply to a quick run and which to a workflow run", () => {
    setup();
    const limits = screen.getByRole("heading", { name: "Limits" }).parentElement!;
    expect(limits.textContent).toContain("A quick run stops when it reaches any of these limits; workflow runs use the workflow's limits, plus this agent's cost cap.");
    expect(limits.textContent).not.toContain("A run stops when it reaches any limit");
  });
});
