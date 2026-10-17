// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { ChannelsInfo, Project, Schedule, Status } from "@/lib/api";
import { clearToken } from "@/lib/auth";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import StatusPage from "./StatusPage";

const baseStatus: Status = { version: "0.3.0", uptime_s: 3600, db_ok: true, workers: 4, sse_clients: 1, runs: {} };
const schedule = (id: string, over: Partial<Schedule>): Schedule => ({
  id,
  workflow_id: "w1",
  kind: "cron",
  cron_expr: "0 9 * * *",
  tz: "UTC",
  enabled: true,
  input: {},
  next_run_at: "2026-10-16T09:00:00Z",
  created_at: "2026-10-14T09:00:00Z",
  updated_at: "2026-10-14T09:00:00Z",
  ...over,
});

let status: Status;
let channels: ChannelsInfo;
let schedules: Schedule[];
let projects: Project[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  status = baseStatus;
  channels = { telegram: { enabled: true, mode: "polling", bot_username: "yuno_bot", allowed_user_ids: [1], binding: null } };
  schedules = [
    schedule("s1", { next_run_at: "2026-10-17T09:00:00Z" }),
    schedule("s2", { next_run_at: "2026-10-16T09:00:00Z" }),
    schedule("s3", { enabled: false, next_run_at: "2026-10-15T20:00:00Z" }),
  ];
  projects = [];
  api = installFakeApi();
  api.on("GET", "/api/status", () => status);
  api.on("GET", "/api/projects", () => projects);
  api.on("GET", "/api/openclaw/status", () => ({ connected: true, version: "2026.10.3", scopes: ["operator.read"], agents: 3 }));
  api.on("GET", "/api/channels", () => channels);
  api.on("GET", "/api/schedules", () => schedules);
});

afterEach(() => {
  cleanup();
  clearToken();
  localStorage.clear();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <StatusPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const panel = async (name: string) => within((await screen.findByRole("heading", { name })).closest("section") as HTMLElement);
const value = (p: ReturnType<typeof within>, label: string) => p.getByText(label).nextElementSibling!.textContent;

describe("OpenClaw panel", () => {
  it("shows the gateway and whether Telegram is on", async () => {
    renderPage();
    const p = await panel("OpenClaw");
    expect(await p.findByText("Connected")).not.toBeNull();
    expect(p.getByText("2026.10.3")).not.toBeNull();
    expect(p.getByText("operator.read")).not.toBeNull();
    expect(await p.findByText("Enabled (polling)")).not.toBeNull();
  });

  it("shows Telegram as disabled", async () => {
    channels = { telegram: { ...channels.telegram, enabled: false, mode: "off" } };
    renderPage();
    const p = await panel("OpenClaw");
    expect(await p.findByText("Disabled")).not.toBeNull();
  });

  it("says when the channel status cannot be read", async () => {
    api.on("GET", "/api/channels", () => apiError(500, "internal", "boom"));
    renderPage();
    const p = await panel("OpenClaw");
    expect(await p.findByText("Unavailable")).not.toBeNull();
    expect(p.getByText("Connected")).not.toBeNull();
  });
});

describe("Scheduler panel", () => {
  it("counts the schedules that are on and shows the earliest next run among them", async () => {
    renderPage();
    const p = await panel("Scheduler");
    expect(await p.findByText("2")).not.toBeNull();
    expect(value(p, "Active schedules")).toBe("2");
    expect(value(p, "Next due")).toBe(formatDateTime("2026-10-16T09:00:00Z"));
    expect(p.queryByText("Checks for due schedules")).toBeNull();
  });

  it("prefers the scheduler's own next due time and shows how often it checks", async () => {
    status = { ...baseStatus, scheduler: { tick_seconds: 10, next_due_at: "2026-10-15T23:00:00Z" } };
    renderPage();
    const p = await panel("Scheduler");
    await p.findByText("Checks for due schedules");
    expect(value(p, "Next due")).toBe(formatDateTime("2026-10-15T23:00:00Z"));
    expect(value(p, "Checks for due schedules")).toBe("Every 10s");
  });

  it("says when nothing is scheduled", async () => {
    schedules = [];
    renderPage();
    const p = await panel("Scheduler");
    expect(await p.findByText("Nothing scheduled")).not.toBeNull();
    expect(value(p, "Active schedules")).toBe("0");
  });
});

describe("platform cards", () => {
  it("puts the api version first, ahead of the other figures and the OpenClaw panel", async () => {
    status = { ...baseStatus, version: "0.3.0+1941f75" };
    renderPage();
    const version = (await screen.findAllByText("Version"))[0];
    expect(version.nextElementSibling!.textContent).toBe("0.3.0+1941f75");
    expect(version.compareDocumentPosition(screen.getByText("Uptime")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const openclaw = await screen.findByRole("heading", { name: "OpenClaw" });
    expect(version.compareDocumentPosition(openclaw) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

describe("API token", () => {
  it("says a token is in use, without showing it", async () => {
    localStorage.setItem("yuno.apiToken", "tok-never-shown-0001");
    renderPage();
    expect(await screen.findByText("API token in use")).not.toBeNull();
    expect(screen.queryByText(/tok-never-shown-0001/)).toBeNull();
    expect(document.body.innerHTML).not.toContain("tok-never-shown-0001");
  });

  it("says when none is", async () => {
    renderPage();
    expect(await screen.findByText("No API token")).not.toBeNull();
    expect(screen.queryByText("API token in use")).toBeNull();
  });
});

describe("Projects panel", () => {
  it("lists the projects with their repository, provider, branch and network, and links to the Projects page", async () => {
    projects = [
      project({}),
      project({ id: "p2", name: "docs-site", repo_url: "https://github.com/acme/docs", provider: "github", default_branch: "develop", sandbox_network: true }),
    ];
    renderPage();
    const p = await panel("Projects");
    const first = within((await p.findByText("yuno-playground")).closest("tr") as HTMLElement).getAllByRole("cell");
    expect(first.map((c) => c.textContent)).toEqual(["yuno-playground", "gitlab.com/andrebatista/yuno-playground.git", "GitLab", "main", "Network off"]);
    const second = within(p.getByText("docs-site").closest("tr") as HTMLElement).getAllByRole("cell");
    expect(second.map((c) => c.textContent)).toEqual(["docs-site", "github.com/acme/docs", "GitHub", "develop", "Network on"]);
    expect(p.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Name", "Repository", "Provider", "Branch", "Sandbox network"]);
    expect(p.getByRole("link", { name: "Manage projects" }).getAttribute("href")).toBe("/projects");
    expect(p.queryByText(/None yet/)).toBeNull();
  });

  it("says there are none yet, with a way to the Projects page", async () => {
    renderPage();
    const p = await panel("Projects");
    const note = await p.findByText(/None yet/);
    expect(note.textContent).toBe("None yet. Create one on the Projects page.");
    expect(within(note).getByRole("link", { name: "Projects page" }).getAttribute("href")).toBe("/projects");
    expect(p.queryByRole("table")).toBeNull();
  });

  it("replaces the single project repository panel and no longer reads the project from the status", async () => {
    status = { ...baseStatus, project: { configured: true, repo_url: "https://gitlab.com/g/p.git" } } as Status;
    renderPage();
    await panel("Projects");
    expect(screen.queryByRole("heading", { name: "Project repository" })).toBeNull();
    expect(screen.queryByText("https://gitlab.com/g/p.git")).toBeNull();
  });

  it("says when the projects cannot be loaded, without touching the other panels", async () => {
    api.on("GET", "/api/projects", () => apiError(500, "internal", "boom"));
    renderPage();
    const p = await panel("Projects");
    expect((await p.findByRole("alert")).textContent).toContain("Could not load the projects: boom");
    expect(await (await panel("OpenClaw")).findByText("Connected")).not.toBeNull();
  });
});
