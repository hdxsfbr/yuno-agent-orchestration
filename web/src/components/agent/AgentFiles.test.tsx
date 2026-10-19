// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { AgentFile, AgentFileEdit } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import AgentFiles from "./AgentFiles";

const file = (over: Partial<AgentFile>): AgentFile => ({ name: "SOUL.md", kind: "generated", exists: true, content: "# Soul\n\nBe careful.", addition: null, ...over });

let files: AgentFile[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  files = [
    file({}),
    file({ name: "AGENTS.md", content: "# Agents" }),
    file({ name: "IDENTITY.md", kind: "runtime", content: "I am Coder", override: null }),
    file({ name: "USER.md", kind: "runtime", exists: false, content: "", override: null }),
    file({ name: "TOOLS.md", kind: "runtime", content: "tools notes", override: "my tools" }),
    file({ name: "DREAMS.md", kind: "runtime", content: "long", truncated: true, override: null }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/agents/a1/files", () => files);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderFiles() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <AgentFiles agentId="a1" />
    </QueryClientProvider>,
  );
}

const row = async (name: string) => within((await screen.findByText(name, { selector: "li span" })).closest("li") as HTMLElement);

describe("AgentFiles list", () => {
  it("shows every file with its kind and state", async () => {
    renderFiles();
    const soul = await row("SOUL.md");
    expect(soul.getByText("Written by the platform")).not.toBeNull();
    const user = await row("USER.md");
    expect(user.getByText("Runtime file")).not.toBeNull();
    expect(user.getByText("Not created yet")).not.toBeNull();
    expect(user.getByRole("button", { name: "Create USER.md" })).not.toBeNull();
    expect((await row("TOOLS.md")).getByText("Overridden")).not.toBeNull();
    expect((await row("DREAMS.md")).getByText("Truncated")).not.toBeNull();
  });
});

describe("generated files", () => {
  it("shows the current content read-only and saves the addition", async () => {
    api.on("PUT", "/api/agents/a1/files/SOUL.md", (r) => file({ addition: (r.body as AgentFileEdit).addition }));
    renderFiles();
    fireEvent.click((await row("SOUL.md")).getByRole("button", { name: "Open SOUL.md" }));
    const current = (await screen.findByLabelText("Current content")) as HTMLTextAreaElement;
    expect(current.readOnly).toBe(true);
    expect(current.value).toBe("# Soul\n\nBe careful.");
    expect(screen.getByText(/keeps its own part/)).not.toBeNull();

    fireEvent.change(screen.getByLabelText("Your additions"), { target: { value: "Prefer small diffs." } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1/files/SOUL.md")).toHaveLength(1));
    expect(api.made("PUT", "/api/agents/a1/files/SOUL.md")[0].body).toEqual({ addition: "Prefer small diffs." });
    expect((await screen.findByRole("status")).textContent).toBe("Saved.");
  });

  it("says the edit is saved when the runtime cannot be reached (502)", async () => {
    api.on("PUT", "/api/agents/a1/files/SOUL.md", () => apiError(502, "materialize_failed", "gateway down"));
    renderFiles();
    fireEvent.click((await row("SOUL.md")).getByRole("button", { name: "Open SOUL.md" }));
    fireEvent.change(await screen.findByLabelText("Your additions"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect((await screen.findByRole("status")).textContent).toContain("apply when the runtime is reachable");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows a 400 and a 409 as refusals", async () => {
    api.on("PUT", "/api/agents/a1/files/SOUL.md", () => apiError(409, "conflict", "not materialized"));
    renderFiles();
    fireEvent.click((await row("SOUL.md")).getByRole("button", { name: "Open SOUL.md" }));
    fireEvent.change(await screen.findByLabelText("Your additions"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect((await screen.findByRole("alert")).textContent).toContain("not materialized");
  });

  it("blocks a save over 64 KiB", async () => {
    renderFiles();
    fireEvent.click((await row("SOUL.md")).getByRole("button", { name: "Open SOUL.md" }));
    fireEvent.change(await screen.findByLabelText("Your additions"), { target: { value: "a".repeat(64 * 1024 + 1) } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/over the 64 KiB limit/)).not.toBeNull();
  });
});

describe("runtime files", () => {
  it("sets an override and creates a file that does not exist", async () => {
    api.on("PUT", /^\/api\/agents\/a1\/files\/[^/]+$/, (r) => file({ kind: "runtime", override: (r.body as AgentFileEdit).override }));
    renderFiles();
    fireEvent.click((await row("USER.md")).getByRole("button", { name: "Create USER.md" }));
    expect(await screen.findByText(/does not exist yet/)).not.toBeNull();
    fireEvent.change(screen.getByLabelText("Content"), { target: { value: "The operator is Andre." } });
    fireEvent.click(screen.getByRole("button", { name: "Create file" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1/files/USER.md")).toHaveLength(1));
    expect(api.made("PUT", "/api/agents/a1/files/USER.md")[0].body).toEqual({ override: "The operator is Andre." });
  });

  it("starts from what is on disk and stores it as the override", async () => {
    api.on("PUT", "/api/agents/a1/files/IDENTITY.md", () => file({ kind: "runtime", override: "I am Coder, v2" }));
    renderFiles();
    fireEvent.click((await row("IDENTITY.md")).getByRole("button", { name: "Open IDENTITY.md" }));
    const box = (await screen.findByLabelText("Content")) as HTMLTextAreaElement;
    expect(box.value).toBe("I am Coder");
    fireEvent.change(box, { target: { value: "I am Coder, v2" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1/files/IDENTITY.md")[0]?.body).toEqual({ override: "I am Coder, v2" }));
  });

  it("stops overriding after a confirmation", async () => {
    api.on("PUT", "/api/agents/a1/files/TOOLS.md", () => undefined);
    renderFiles();
    fireEvent.click((await row("TOOLS.md")).getByRole("button", { name: "Open TOOLS.md" }));
    expect(((await screen.findByLabelText("Content")) as HTMLTextAreaElement).value).toBe("my tools");
    fireEvent.click(screen.getByRole("button", { name: "Stop overriding" }));
    const d = within(await screen.findByRole("dialog"));
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(api.made("PUT", /TOOLS/)).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Stop overriding" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Stop overriding" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1/files/TOOLS.md")).toHaveLength(1));
    expect(api.made("PUT", "/api/agents/a1/files/TOOLS.md")[0].body).toEqual({ override: null });
  });

  it("does not offer to edit a file with hidden secrets, but can stop overriding", async () => {
    files.push(file({ name: "HEARTBEAT.md", kind: "runtime", content: "token=***", redacted: true, override: "token=***" }));
    api.on("PUT", "/api/agents/a1/files/HEARTBEAT.md", () => undefined);
    renderFiles();
    fireEvent.click((await row("HEARTBEAT.md")).getByRole("button", { name: "Open HEARTBEAT.md" }));
    expect(await screen.findByText("This file contains secrets that are hidden here, so it can't be edited from this page.")).not.toBeNull();
    expect(screen.queryByLabelText("Content")).toBeNull();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Stop overriding" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Stop overriding" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1/files/HEARTBEAT.md")[0]?.body).toEqual({ override: null }));
  });

  it("says a truncated file is longer than shown", async () => {
    renderFiles();
    fireEvent.click((await row("DREAMS.md")).getByRole("button", { name: "Open DREAMS.md" }));
    expect(await screen.findByText(/longer than shown/)).not.toBeNull();
  });
});

describe("unsaved changes", () => {
  it("asks before another file replaces the text being edited", async () => {
    renderFiles();
    fireEvent.click((await row("SOUL.md")).getByRole("button", { name: "Open SOUL.md" }));
    fireEvent.change(await screen.findByLabelText("Your additions"), { target: { value: "draft" } });
    expect(screen.getByText("Unsaved changes")).not.toBeNull();
    fireEvent.click((await row("AGENTS.md")).getByRole("button", { name: "Open AGENTS.md" }));
    const d = within(await screen.findByRole("dialog"));
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect((screen.getByLabelText("Your additions") as HTMLTextAreaElement).value).toBe("draft");

    fireEvent.click(screen.getByRole("button", { name: "Open AGENTS.md" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect((screen.getByLabelText("Current content") as HTMLTextAreaElement).value).toBe("# Agents"));
  });
});
