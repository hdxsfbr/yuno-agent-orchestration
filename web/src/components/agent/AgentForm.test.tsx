// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import type { AgentInput, Tool } from "@/lib/api";
import type { FieldErrors } from "@/lib/agentForm";
import AgentForm from "./AgentForm";

afterEach(cleanup);

const tools: Tool[] = [
  { name: "read", description: "Read a file", risk: "read_only", hits: "none", timeout_seconds: 10, source: "openclaw" },
  { name: "deploy_release", description: "Deploy a release", risk: "side_effecting", hits: "external", timeout_seconds: 60, source: "platform" },
  { name: "memory_recall", description: "Recall facts", risk: "read_only", hits: "postgres", timeout_seconds: 10, source: "platform" },
];

function setup(over: { serverErrors?: FieldErrors; formError?: string } = {}) {
  const onSubmit = vi.fn<(input: AgentInput) => void>();
  render(
    <MemoryRouter>
      <AgentForm
        tools={{ data: tools, isLoading: false, error: null, refetch: () => undefined }}
        saving={false}
        serverErrors={over.serverErrors ?? {}}
        formError={over.formError}
        submitLabel="Create agent"
        onSubmit={onSubmit}
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
  it("adds and removes skills and rejects an invalid name", () => {
    const { onSubmit } = setup();
    const skills = screen.getByLabelText("Skills");
    fireEvent.change(skills, { target: { value: "Bad Name" } });
    fireEvent.click(within(skills.parentElement!).getByRole("button", { name: "Add" }));
    expect(screen.getByText("Use lowercase words separated by hyphens")).not.toBeNull();

    fireEvent.change(skills, { target: { value: "code-review" } });
    fireEvent.keyDown(skills, { key: "Enter" });
    expect(screen.getByRole("button", { name: "Remove code-review" })).not.toBeNull();

    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    submit();
    expect(onSubmit.mock.calls[0][0].skills).toEqual(["code-review"]);

    fireEvent.click(screen.getByRole("button", { name: "Remove code-review" }));
    expect(screen.queryByRole("button", { name: "Remove code-review" })).toBeNull();
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

  it("sets Telegram and the exec policy", () => {
    const { onSubmit } = setup();
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Scout" } });
    fireEvent.click(screen.getByLabelText(/^Telegram/));
    fireEvent.change(screen.getByLabelText("Security"), { target: { value: "full" } });
    const allow = screen.getByLabelText("Allowlist");
    fireEvent.change(allow, { target: { value: "git status" } });
    fireEvent.keyDown(allow, { key: "Enter" });
    submit();
    const input = onSubmit.mock.calls[0][0];
    expect(input.channels).toEqual(["telegram"]);
    expect(input.exec_policy).toMatchObject({ security: "full", allowlist: ["git status"] });
  });
});
