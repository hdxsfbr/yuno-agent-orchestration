// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Agent } from "@/lib/api";
import { installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import AgentsPage from "./AgentsPage";

const agent = (over: Partial<Agent>): Agent => ({
  id: "a1",
  name: "Coder",
  model: "claude-opus-5-5",
  openclaw_agent_id: "agb79ae974ba",
  tool_allowlist: ["read"],
  channels: [],
  activity: { running: 0, waiting_approval: 0 },
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T09:00:00Z",
  ...over,
});

let agents: Agent[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  agents = [agent({})];
  api = installFakeApi();
  api.on("GET", "/api/agents", () => agents);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <AgentsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const cells = async (name: string) => within(((await screen.findByRole("link", { name })).closest("tr")) as HTMLElement).getAllByRole("cell");
const column = (header: string) => screen.getAllByRole("columnheader").findIndex((h) => h.textContent === header);
const listCalls = () => api.made("GET", "/api/agents").length;
const sleep = (ms: number) => act(async () => void (await new Promise((r) => setTimeout(r, ms))));
const streamOpen = () => waitFor(() => expect(api.hasStream("/api/events")).toBe(true));

describe("Model column", () => {
  it("shows each agent's model as it is configured, an OpenAI one with its provider", async () => {
    agents = [agent({}), agent({ id: "a2", name: "Builder", model: "openai/gpt-6-astra" })];
    renderPage();
    const coder = await cells("Coder");
    const model = column("Model");
    expect(coder[model].textContent).toBe("claude-opus-5-5");
    expect((await cells("Builder"))[model].textContent).toBe("openai/gpt-6-astra");
  });
});

describe("Channel column", () => {
  it("names the channel an agent is reached on, and shows a dash for one that has none", async () => {
    agents = [agent({ channels: ["telegram"] }), agent({ id: "a2", name: "Reviewer", channels: [] })];
    renderPage();
    const coder = await cells("Coder");
    const channel = column("Channel");
    expect(channel).toBeGreaterThan(-1);
    expect(coder[channel].textContent).toBe("Telegram");
    expect((await cells("Reviewer"))[channel].textContent).toBe("—");
  });

  it("shows a dash when the list leaves the channels out", async () => {
    agents = [agent({ channels: undefined })];
    renderPage();
    expect((await cells("Coder"))[column("Channel")].textContent).toBe("—");
  });
});

describe("State column", () => {
  it("shows Running with the number of runs, Waiting with the number, or Idle", async () => {
    agents = [
      agent({ activity: { running: 2, waiting_approval: 0 } }),
      agent({ id: "a2", name: "Reviewer", activity: { running: 0, waiting_approval: 1 } }),
      agent({ id: "a3", name: "Scout", activity: { running: 0, waiting_approval: 0 } }),
    ];
    renderPage();
    const state = (name: string) => cells(name).then((c) => c[column("State")].textContent);
    expect(await state("Coder")).toBe("Running (2)");
    expect(await state("Reviewer")).toBe("Waiting (1)");
    expect(await state("Scout")).toBe("Idle");
  });

  it("shows both counts when an agent has runs going and others waiting for a decision", async () => {
    agents = [agent({ activity: { running: 1, waiting_approval: 3 } })];
    renderPage();
    const text = (await cells("Coder"))[column("State")].textContent;
    expect(text).toContain("Running (1)");
    expect(text).toContain("Waiting (3)");
    expect(text).not.toContain("Idle");
  });

  it("is Idle for an agent the list reports no activity for", async () => {
    agents = [agent({ activity: undefined })];
    renderPage();
    expect((await cells("Coder"))[column("State")].textContent).toBe("Idle");
  });

  it("still warns about an agent that is not materialised", async () => {
    agents = [agent({ openclaw_agent_id: undefined })];
    renderPage();
    const text = (await cells("Coder"))[column("State")].textContent;
    expect(text).toContain("Not materialised");
    expect(text).toContain("Idle");
  });
});

describe("live state", () => {
  it("shows that the list is live", async () => {
    renderPage();
    expect(await screen.findByText("Live")).not.toBeNull();
  });

  it.each(["run.status", "node.status"])("refreshes the agents when a %s event arrives", async (event) => {
    renderPage();
    expect((await cells("Coder"))[column("State")].textContent).toBe("Idle");
    await streamOpen();

    agents = [agent({ activity: { running: 1, waiting_approval: 0 } })];
    api.send("/api/events", event, { run: { id: "r1", status: "running" } });

    await waitFor(() => expect(screen.getByText("Running (1)")).not.toBeNull());
    expect(listCalls()).toBe(2);
  });

  it("ignores events that cannot change what an agent is doing", async () => {
    renderPage();
    await cells("Coder");
    await streamOpen();
    for (const event of ["approval.requested", "conversation.message", "log", "tool_call", "model_call"]) api.send("/api/events", event, {});
    await sleep(800);
    expect(listCalls()).toBe(1);

    api.send("/api/events", "run.status", {});
    await waitFor(() => expect(listCalls()).toBe(2));
  });

  it("asks once for a burst of events, not once per event", async () => {
    renderPage();
    await cells("Coder");
    await streamOpen();
    for (let i = 0; i < 6; i++) api.send("/api/events", i % 2 ? "node.status" : "run.status", {});
    // the list is asked for after a short pause, not on the first event
    await sleep(150);
    expect(listCalls()).toBe(1);
    await waitFor(() => expect(listCalls()).toBe(2));
    await sleep(800);
    expect(listCalls()).toBe(2);
  });
});

describe("quick run", () => {
  it("offers the projects to work on once there are some", async () => {
    api.on("GET", "/api/projects", () => [project({}), project({ id: "p2", name: "docs-site" })]);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Run Coder" }));
    const select = (await dialog.findByLabelText("Project")) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["None", "yuno-playground", "docs-site"]);
  });

  it("shows no project choice while there are no projects", async () => {
    api.on("GET", "/api/projects", () => []);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Run Coder" }));
    await waitFor(() => expect(api.made("GET", "/api/projects")).toHaveLength(1));
    expect(dialog.queryByLabelText("Project")).toBeNull();
  });
});
