// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { Agent, AgentInput, ChannelsInfo, MemoryFact, Skill } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import AgentFormPage from "./AgentFormPage";

const agent = (over: Partial<Agent> = {}): Agent => ({
  id: "a1",
  name: "Coder",
  model: "claude-opus-5-5",
  openclaw_agent_id: "agb79ae974ba",
  memory_policy: { enabled: true },
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T09:00:00Z",
  ...over,
});

const fact = (over: Partial<MemoryFact>): MemoryFact => ({
  id: "f1",
  key: "preferred_language",
  value: "Go, with table-driven tests",
  created_at: "2026-10-14T09:00:00Z",
  expires_at: "2026-11-13T09:00:00Z",
  ...over,
});

const skill = (id: string, name: string, description: string): Skill => ({ id, name, description, body: "x", created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T09:00:00Z" });
const channels = (binding: ChannelsInfo["telegram"]["binding"]): ChannelsInfo => ({ telegram: { enabled: true, mode: "polling", allowed_user_ids: [1], binding } });

let saved: Agent;
let facts: MemoryFact[];
let info: ChannelsInfo;
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  saved = agent();
  facts = [fact({}), fact({ id: "f2", key: "deploy_day", value: "Fridays are frozen", expires_at: null })];
  info = channels(null);
  api = installFakeApi();
  api.on("GET", "/api/agents/a1", () => saved);
  api.on("GET", "/api/agents/a1/runtime", () => ({ openclaw_agent_id: "agb79ae974ba", materialized: true, in_sync: true }));
  api.on("GET", "/api/agents", () => [saved]);
  api.on("GET", "/api/tools", () => []);
  api.on("GET", "/api/skills", () => [skill("s1", "code-review", "Reviews diffs"), skill("s2", "release-notes", "")]);
  api.on("GET", "/api/agents/a1/memory", () => facts);
  api.on("GET", "/api/channels", () => info);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/agents/a1"]}>
        <Routes>
          <Route path="/agents/:id" element={<AgentFormPage />} />
          <Route path="/channels" element={<p>channels page</p>} />
          <Route path="/skills" element={<p>skills page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const memory = async () => within((await screen.findByRole("heading", { name: "Saved facts" })).closest("section") as HTMLElement);

describe("Memory section", () => {
  it("lists the saved facts with when they were saved and when they expire", async () => {
    renderPage();
    const m = await memory();
    const first = (await m.findByText("preferred_language")).closest("li") as HTMLElement;
    expect(within(first).getByText("Go, with table-driven tests")).not.toBeNull();
    expect(within(first).getByText(`Saved ${formatDateTime("2026-10-14T09:00:00Z")} · Expires ${formatDateTime("2026-11-13T09:00:00Z")}`)).not.toBeNull();
    const second = m.getByText("deploy_day").closest("li") as HTMLElement;
    expect(within(second).getByText(/Does not expire/)).not.toBeNull();
  });

  it("deletes a fact and takes it off the list", async () => {
    api.on("DELETE", /^\/api\/agents\/a1\/memory\/([^/]+)$/, (r) => {
      facts = facts.filter((f) => f.id !== r.match[1]);
      return new Response(null, { status: 204 });
    });
    renderPage();
    const m = await memory();
    fireEvent.click(await m.findByRole("button", { name: "Delete preferred_language" }));
    await waitFor(() => expect(m.queryByText("preferred_language")).toBeNull());
    expect(api.made("DELETE", "/api/agents/a1/memory/f1")).toHaveLength(1);
    expect(m.getByText("deploy_day")).not.toBeNull();
  });

  it("keeps the fact and says why when the delete fails", async () => {
    api.on("DELETE", /^\/api\/agents\/a1\/memory\/[^/]+$/, () => apiError(404, "not_found", "fact not found"));
    renderPage();
    const m = await memory();
    fireEvent.click(await m.findByRole("button", { name: "Delete preferred_language" }));
    expect((await m.findByRole("alert")).textContent).toContain("fact not found");
    expect(m.getByText("preferred_language")).not.toBeNull();
  });

  it("says there are no facts yet while memory is on", async () => {
    facts = [];
    renderPage();
    const m = await memory();
    expect(await m.findByText("No facts saved yet.")).not.toBeNull();
  });

  it("says memory is off for an agent whose policy is disabled", async () => {
    saved = agent({ memory_policy: { enabled: false } });
    facts = [];
    renderPage();
    const m = await memory();
    expect(await m.findByText(/Memory is turned off for this agent/)).not.toBeNull();
    expect(m.queryByText("No facts saved yet.")).toBeNull();
  });
});

describe("Telegram line", () => {
  it("says this agent is bound and links to Channels", async () => {
    info = channels({ agent_id: "a1", agent_name: "Coder" });
    renderPage();
    expect(await screen.findByText("Bound to this agent")).not.toBeNull();
    fireEvent.click(screen.getByRole("link", { name: "Manage in Channels" }));
    expect(await screen.findByText("channels page")).not.toBeNull();
  });

  it("names the agent that is bound instead", async () => {
    info = channels({ agent_id: "a2", agent_name: "Reviewer" });
    renderPage();
    expect(await screen.findByText("Bound to Reviewer")).not.toBeNull();
  });

  it("says when no agent is bound", async () => {
    renderPage();
    expect(await screen.findByText("Not bound")).not.toBeNull();
  });
});

describe("saving", () => {
  it("sends the skills ticked and keeps the agent's channels", async () => {
    saved = agent({ skills: ["code-review"], channels: ["telegram"] });
    api.on("PUT", "/api/agents/a1", (r) => ({ ...saved, ...(r.body as AgentInput) }));
    renderPage();
    const group = within(await screen.findByRole("group", { name: "Skills" }));
    await group.findByRole("checkbox", { name: /release-notes/ });
    expect((group.getByRole("checkbox", { name: /code-review/ }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(group.getByRole("checkbox", { name: /release-notes/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.made("PUT", "/api/agents/a1")).toHaveLength(1));
    const body = api.made("PUT", "/api/agents/a1")[0].body as AgentInput;
    expect(body.skills).toEqual(["code-review", "release-notes"]);
    expect(body.channels).toEqual(["telegram"]);
  });

  it("puts a bad guardrail expression's 422 next to that pattern", async () => {
    saved = agent({ guardrails: { blocked_input_patterns: [{ name: "no-rm", tool: "exec", regex: "rm -rf" }, { name: "odd", tool: "exec", regex: "(" }] } });
    api.on("PUT", "/api/agents/a1", () =>
      apiError(422, "validation_failed", "validation failed", [{ path: "guardrails.blocked_input_patterns[1].pattern", message: "error parsing regexp: missing closing ): `(`" }]),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    const bad = await screen.findByLabelText("Pattern 2 regex");
    await waitFor(() => expect(bad.parentElement?.textContent).toContain("missing closing )"));
    expect(bad.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByLabelText("Pattern 1 regex").getAttribute("aria-invalid")).toBeNull();
  });
});
