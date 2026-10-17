// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { ApprovalItem } from "@/lib/api";
import { clearToken } from "@/lib/auth";
import { installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import App from "./App";

const pending = (id: string): ApprovalItem => ({
  id,
  run_id: "r1",
  run_node_id: "n1",
  kind: "node",
  node_id: "gate",
  message: "Deploy?",
  status: "pending",
  requested_at: "2026-10-15T10:00:00Z",
  expires_at: "2026-10-16T10:00:00Z",
});

let waiting: ApprovalItem[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  waiting = [pending("a1"), pending("a2")];
  api = installFakeApi();
  api.on("GET", "/api/workflows", () => []);
  api.on("GET", /^\/api\/approvals\?status=pending$/, () => waiting);
});

afterEach(() => {
  cleanup();
  clearToken();
  localStorage.clear();
  vi.unstubAllGlobals();
});

function renderApp(path = "/workflows") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("navigation", () => {
  it("links to every page, Projects, Channels, Schedules and Skills included", async () => {
    renderApp();
    const nav = within(screen.getByRole("navigation"));
    expect(nav.getAllByRole("link").map((l) => l.getAttribute("href"))).toEqual(["/agents", "/workflows", "/projects", "/runs", "/approvals", "/channels", "/schedules", "/skills", "/status"]);
    expect(await screen.findByRole("heading", { name: "Workflows" })).not.toBeNull();
  });

  it("serves the Projects page at /projects and opens a project's form at /projects/<id>", async () => {
    api.on("GET", "/api/projects", () => [project({})]);
    renderApp("/projects");
    expect(await screen.findByRole("heading", { name: "Projects" })).not.toBeNull();
    expect(await screen.findByRole("link", { name: "yuno-playground" })).not.toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    cleanup();

    renderApp("/projects/p1");
    expect((await screen.findByRole("dialog", { name: "Edit yuno-playground" })).textContent).toContain("A token is set. Enter a new one to replace it.");
  });

  it("badges Approvals with the number of requests waiting and keeps it current", async () => {
    renderApp();
    const link = screen.getByRole("link", { name: /^Approvals/ });
    expect((await within(link).findByLabelText("2 pending")).textContent).toBe("2");

    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    waiting = [pending("a2")];
    api.send("/api/events", "approval.decided", { approval: { ...pending("a1"), status: "approved" } });
    await waitFor(() => expect(within(link).getByLabelText("1 pending")).not.toBeNull());

    waiting = [pending("a2"), pending("a3"), pending("a4")];
    api.send("/api/events", "approval.requested", { approval: pending("a3") });
    await waitFor(() => expect(within(link).getByLabelText("3 pending")).not.toBeNull());
  });

  it("shows no badge when nothing is waiting", async () => {
    waiting = [];
    renderApp();
    await waitFor(() => expect(api.made("GET", /approvals/).length).toBeGreaterThan(0));
    expect(within(screen.getByRole("link", { name: "Approvals" })).queryByLabelText(/pending/)).toBeNull();
  });
});

describe("API token", () => {
  const TOKEN = "platform-secret-0001";
  const bearer = `Bearer ${TOKEN}`;
  const dialog = () => screen.findByRole("dialog", { name: "Sign in" });
  const signIn = (value: string) => {
    fireEvent.change(screen.getByLabelText("Platform token"), { target: { value } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
  };

  it("sends the stored token on every request and on the stream, without asking", async () => {
    localStorage.setItem("yuno.apiToken", TOKEN);
    api.requireToken(TOKEN);
    renderApp();
    expect((await screen.findByRole("heading", { name: "Workflows" })).textContent).toBe("Workflows");
    await within(screen.getByRole("link", { name: /^Approvals/ })).findByLabelText("2 pending");
    await waitFor(() => expect(api.streamCalls.length).toBeGreaterThan(0));
    expect(screen.queryByRole("dialog")).toBeNull();
    for (const c of [...api.calls, ...api.streamCalls]) expect(c.headers.authorization).toBe(bearer);
  });

  it("opens Sign in on a 401 and, once the token is saved, retries what failed with the bearer on it and on the stream", async () => {
    api.requireToken(TOKEN);
    renderApp();
    const modal = within(await dialog());
    expect((modal.getByLabelText("Platform token") as HTMLInputElement).type).toBe("password");
    expect(api.made("GET", "/api/workflows").every((c) => c.headers.authorization === undefined)).toBe(true);

    signIn(`  ${TOKEN}  `);

    expect(localStorage.getItem("yuno.apiToken")).toBe(TOKEN);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(api.made("GET", "/api/workflows").some((c) => c.headers.authorization === bearer)).toBe(true));
    // the approvals badge only appears if its failed query was asked again too
    expect((await within(screen.getByRole("link", { name: /^Approvals/ })).findByLabelText("2 pending")).textContent).toBe("2");
    await waitFor(() => expect(api.streamCalls.some((c) => c.headers.authorization === bearer)).toBe(true));
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    // the token travels in a header only
    for (const c of [...api.calls, ...api.streamCalls]) expect(c.url).not.toContain(TOKEN);
  });

  it("says when the token was not accepted and asks again", async () => {
    api.requireToken(TOKEN);
    renderApp();
    await dialog();
    expect(screen.queryByText(/did not accept that token/)).toBeNull();

    signIn("not-the-token");
    expect((await screen.findByText(/did not accept that token/)).textContent).toContain("Check it and try again");
    expect(screen.getByLabelText("Platform token").getAttribute("aria-invalid")).toBe("true");

    signIn(TOKEN);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(localStorage.getItem("yuno.apiToken")).toBe(TOKEN);
  });

  it("will not save an empty token", async () => {
    api.requireToken(TOKEN);
    renderApp();
    await dialog();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Platform token"), { target: { value: "   " } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    // Enter in the field submits the form even when the button is disabled
    fireEvent.submit(screen.getByLabelText("Platform token").closest("form")!);
    expect(screen.getByRole("dialog", { name: "Sign in" })).not.toBeNull();
    expect(localStorage.getItem("yuno.apiToken")).toBeNull();
  });

  it("signs out from the nav footer: the token is removed and Sign in returns", async () => {
    localStorage.setItem("yuno.apiToken", TOKEN);
    api.requireToken(TOKEN);
    renderApp();
    await within(screen.getByRole("link", { name: /^Approvals/ })).findByLabelText("2 pending");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    const streamsBefore = api.streamCalls.length;

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    expect(localStorage.getItem("yuno.apiToken")).toBeNull();
    await dialog();
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
    // the open stream is dropped and reopened without the old bearer
    await waitFor(() => expect(api.streamCalls.length).toBeGreaterThan(streamsBefore));
    expect(api.streamCalls[api.streamCalls.length - 1].headers.authorization).toBeUndefined();
    expect(api.made("GET", /approvals/).slice(-1)[0].headers.authorization).toBeUndefined();
  });

  it("offers Sign out only while a token is stored", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "Workflows" });
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
  });
});
