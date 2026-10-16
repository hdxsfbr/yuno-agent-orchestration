// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { ChannelsInfo, Schedule, Status } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
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
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  status = baseStatus;
  channels = { telegram: { enabled: true, mode: "polling", bot_username: "yuno_bot", allowed_user_ids: [1], binding: null } };
  schedules = [
    schedule("s1", { next_run_at: "2026-10-17T09:00:00Z" }),
    schedule("s2", { next_run_at: "2026-10-16T09:00:00Z" }),
    schedule("s3", { enabled: false, next_run_at: "2026-10-15T20:00:00Z" }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/status", () => status);
  api.on("GET", "/api/openclaw/status", () => ({ connected: true, version: "2026.10.3", scopes: ["operator.read"], agents: 3 }));
  api.on("GET", "/api/channels", () => channels);
  api.on("GET", "/api/schedules", () => schedules);
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
