// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { ApprovalItem } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import ApprovalsPage from "./ApprovalsPage";

const RUN = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";

const approval = (over: Partial<ApprovalItem>): ApprovalItem => ({
  id: "ap-node",
  run_id: RUN,
  run_node_id: "rn1",
  kind: "node",
  node_id: "gate",
  message: "Deploy release 1.0.0?",
  status: "pending",
  requested_at: "2026-10-15T10:00:00Z",
  expires_at: "2026-10-16T10:00:00Z",
  ...over,
});

const nodeApproval = approval({});
const toolApproval = approval({
  id: "ap-tool",
  kind: "tool",
  node_id: "deployer",
  message: "Deploy release 1.2.0 with deploy_release",
  requested_at: "2026-10-15T10:05:00Z",
  tool_call_id: "tc1",
  tool_call: {
    id: "tc1",
    run_id: RUN,
    run_node_id: "rn2",
    tool_use_id: "mcp-1",
    tool_name: "deploy_release",
    input: { version: "1.2.0", paths: ["app.py", "README.md"] },
    is_error: false,
    status: "awaiting_approval",
    attempt: 1,
    duration_ms: 0,
  },
});
const execApproval = approval({
  id: "ap-exec",
  kind: "exec",
  node_id: "coder",
  message: "rm -rf build/",
  requested_at: "2026-10-15T10:10:00Z",
});

let server: ApprovalItem[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  server = [execApproval, nodeApproval, toolApproval].map((a) => ({ ...a }));
  api = installFakeApi();
  api.on("GET", /^\/api\/approvals(\?.*)?$/, (r) => {
    const status = new URL(r.url, "http://x").searchParams.get("status");
    return server.filter((a) => !status || a.status === status);
  });
  const decide = (status: "approved" | "rejected") => (r: { match: RegExpMatchArray; body: unknown }) => {
    const a = server.find((x) => x.id === r.match[1]);
    if (!a) return apiError(404, "not_found", "approval not found");
    if (a.status !== "pending") return apiError(409, "conflict", "approval already decided");
    a.status = status;
    a.decided_at = "2026-10-15T11:00:00Z";
    a.decided_by = "api";
    a.reason = (r.body as { reason?: string }).reason;
    return a;
  };
  api.on("POST", /^\/api\/approvals\/([^/]+)\/approve$/, decide("approved"));
  api.on("POST", /^\/api\/approvals\/([^/]+)\/reject$/, decide("rejected"));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage(path = "/approvals") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/approvals" element={<ApprovalsPage />} />
          <Route path="/runs/:id" element={<p>run page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const label = (id: string) => `Approval ${shortId(id)}`;
const card = async (id: string) => within(await screen.findByRole("article", { name: label(id) }));

describe("ApprovalsPage", () => {
  it("shows kind, status, run, node, message and the requested and expiry times", async () => {
    renderPage();
    const node = await card("ap-node");
    expect(node.getByText("Workflow step")).not.toBeNull();
    expect(node.getByText("pending")).not.toBeNull();
    expect(node.getByText("Deploy release 1.0.0?")).not.toBeNull();
    expect(node.getByText("gate")).not.toBeNull();
    expect(node.getByText(`Requested ${formatDateTime(nodeApproval.requested_at)}`)).not.toBeNull();
    expect(node.getByText("Expires").nextElementSibling!.textContent).toBe(formatDateTime(nodeApproval.expires_at));
    const link = node.getByRole("link", { name: shortId(RUN) });
    expect(link.getAttribute("href")).toBe(`/runs/${RUN}`);
  });

  it("puts the oldest request first", async () => {
    renderPage();
    await screen.findByText("rm -rf build/");
    // requested at 10:00, 10:05 and 10:10
    expect(screen.getAllByRole("article").map((a) => a.getAttribute("aria-label"))).toEqual(["ap-node", "ap-tool", "ap-exec"].map(label));
  });

  it("shows the tool name and its full input for a tool approval", async () => {
    renderPage();
    const tool = await card("ap-tool");
    expect(tool.getByText("Tool call")).not.toBeNull();
    expect(tool.getByText("deploy_release")).not.toBeNull();
    const input = tool.getByText(/Input to deploy_release/).closest("details")!;
    expect(input.hasAttribute("open")).toBe(true);
    expect(JSON.parse(input.querySelector("pre")!.textContent!)).toEqual({ version: "1.2.0", paths: ["app.py", "README.md"] });
  });

  it("points an approval raised in a conversation to Channels instead of a run", async () => {
    server = [{ ...execApproval, id: "ap-chat", run_id: null }];
    renderPage();
    const chat = await card("ap-chat");
    expect(chat.getByText("Raised in")).not.toBeNull();
    expect(chat.queryByText("Run")).toBeNull();
    expect(chat.getByRole("link", { name: "Conversation" }).getAttribute("href")).toBe("/channels");
  });

  it("offers Approve always for commands only", async () => {
    renderPage();
    expect((await card("ap-exec")).getByRole("button", { name: "Approve always" })).not.toBeNull();
    expect((await card("ap-tool")).queryByRole("button", { name: "Approve always" })).toBeNull();
    expect((await card("ap-node")).queryByRole("button", { name: "Approve always" })).toBeNull();
  });

  it("approves with the reason typed, and the row leaves the list", async () => {
    renderPage();
    const tool = await card("ap-tool");
    fireEvent.change(tool.getByLabelText("Reason"), { target: { value: "version checked" } });
    fireEvent.click(tool.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.queryByText("Deploy release 1.2.0 with deploy_release")).toBeNull());
    expect(api.made("POST", "/api/approvals/ap-tool/approve")[0].body).toEqual({ reason: "version checked" });
    expect(screen.getByText("Deploy release 1.0.0?")).not.toBeNull();
  });

  it("sends no reason when none is typed, and rejects with one", async () => {
    renderPage();
    fireEvent.click((await card("ap-node")).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(api.made("POST", "/api/approvals/ap-node/approve")).toHaveLength(1));
    expect(api.made("POST", "/api/approvals/ap-node/approve")[0].body).toEqual({});

    const tool = await card("ap-tool");
    fireEvent.change(tool.getByLabelText("Reason"), { target: { value: "wrong version" } });
    fireEvent.click(tool.getByRole("button", { name: "Reject" }));
    await waitFor(() => expect(api.made("POST", "/api/approvals/ap-tool/reject")).toHaveLength(1));
    expect(api.made("POST", "/api/approvals/ap-tool/reject")[0].body).toEqual({ reason: "wrong version" });
  });

  it("approves a command for good", async () => {
    renderPage();
    const exec = await card("ap-exec");
    fireEvent.change(exec.getByLabelText("Reason"), { target: { value: "build dir only" } });
    fireEvent.click(exec.getByRole("button", { name: "Approve always" }));
    await waitFor(() => expect(api.made("POST", "/api/approvals/ap-exec/approve")).toHaveLength(1));
    expect(api.made("POST", "/api/approvals/ap-exec/approve")[0].body).toEqual({ reason: "build dir only", always: true });
  });

  it("says the approval was already decided on a 409 and takes the buttons away", async () => {
    renderPage();
    const tool = await card("ap-tool");
    // someone else decided it first
    server.find((a) => a.id === "ap-tool")!.status = "approved";
    fireEvent.click(tool.getByRole("button", { name: "Approve" }));
    expect(await tool.findByText("This approval was already decided.")).not.toBeNull();
    expect(tool.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(tool.queryByRole("button", { name: "Reject" })).toBeNull();
  });

  it("shows another failure as an error and keeps the buttons", async () => {
    api.on("POST", /approve$/, () => apiError(500, "internal", "gateway unreachable"));
    renderPage();
    const node = await card("ap-node");
    fireEvent.click(node.getByRole("button", { name: "Approve" }));
    expect(await node.findByText(/gateway unreachable/)).not.toBeNull();
    expect(node.getByRole("button", { name: "Approve" })).not.toBeNull();
  });

  it("drops a row when the event stream says it was decided, and adds one that is requested", async () => {
    renderPage();
    await screen.findByText("Deploy release 1.0.0?");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    expect(await screen.findByText("Live")).not.toBeNull();

    // decided elsewhere (Telegram, another tab)
    server = server.filter((a) => a.id !== "ap-node");
    api.send("/api/events", "approval.decided", { approval: { ...nodeApproval, status: "approved" } });
    await waitFor(() => expect(screen.queryByText("Deploy release 1.0.0?")).toBeNull());
    expect(screen.getByText("rm -rf build/")).not.toBeNull();

    // a new request arrives
    server = [...server, approval({ id: "ap-new", message: "Open the merge request?", requested_at: "2026-10-15T12:00:00Z" })];
    api.send("/api/events", "approval.requested", { approval: server[server.length - 1] });
    expect(await screen.findByText("Open the merge request?")).not.toBeNull();
  });

  it("adds the ended-run note when the event stream says the approval's run ended", async () => {
    server = [{ ...toolApproval, run_status: "running" }];
    renderPage();
    const tool = await card("ap-tool");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    expect(tool.queryByText(/This run has already ended/)).toBeNull();
    server = [{ ...toolApproval, run_status: "succeeded" }];
    api.send("/api/events", "run.status", { run: { id: toolApproval.run_id, status: "succeeded" } });
    expect(await tool.findByText(/This run has already ended/)).not.toBeNull();
  });

  it("filters by status and shows who decided and why", async () => {
    server = [
      { ...nodeApproval, status: "approved", decided_at: "2026-10-15T10:30:00Z", decided_by: "api", reason: "ship it" },
      { ...toolApproval, status: "rejected", decided_at: "2026-10-15T10:31:00Z", decided_by: "api", reason: "wrong version" },
      execApproval,
    ];
    renderPage();
    await screen.findByText("rm -rf build/");
    expect(screen.getAllByRole("article")).toHaveLength(1);

    fireEvent.change(screen.getByLabelText("Show"), { target: { value: "approved" } });
    const done = await card("ap-node");
    expect(done.getByText("approved")).not.toBeNull();
    expect(done.getByText(/by api · ship it/)).not.toBeNull();
    expect(done.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByText("rm -rf build/")).toBeNull();

    fireEvent.change(screen.getByLabelText("Show"), { target: { value: "all" } });
    await waitFor(() => expect(screen.getAllByRole("article")).toHaveLength(3));
  });

  it("opens on the filter named in the address", async () => {
    server = [{ ...nodeApproval, status: "expired", reason: "run cancelled" }];
    renderPage("/approvals?status=expired");
    expect(await screen.findByText(/run cancelled/)).not.toBeNull();
    expect((screen.getByLabelText("Show") as HTMLSelectElement).value).toBe("expired");
  });

  it("says when nothing is waiting", async () => {
    server = [];
    renderPage();
    expect(await screen.findByText("Nothing is waiting for approval.")).not.toBeNull();
  });
});

describe("approvals of runs that have ended", () => {
  const NOTE = "This run has already ended. Approving still executes the call; rejecting discards it.";

  it.each(["succeeded", "failed", "cancelled", "timed_out"] as const)("tells a pending tool approval whose run is %s that approving still executes the call", async (run_status) => {
    server = [{ ...toolApproval, run_status }];
    renderPage();
    const tool = await card("ap-tool");
    expect(tool.getByRole("note").textContent).toBe(NOTE);
    expect(tool.getByRole("button", { name: "Approve" })).not.toBeNull();
    expect(tool.getByRole("button", { name: "Reject" })).not.toBeNull();
  });

  it("says nothing while the run is still going, or when the inbox does not know its status", async () => {
    server = [
      { ...toolApproval, id: "ap-tool-running", run_status: "running" },
      { ...toolApproval, id: "ap-tool-waiting", run_status: "waiting_approval" },
      { ...toolApproval, id: "ap-tool-unknown", run_status: undefined },
    ];
    renderPage();
    for (const id of ["ap-tool-running", "ap-tool-waiting", "ap-tool-unknown"]) expect((await card(id)).queryByRole("note")).toBeNull();
  });

  it("says nothing for a workflow step or a command, whatever happened to the run", async () => {
    server = [
      { ...nodeApproval, run_status: "failed" },
      { ...execApproval, run_status: "failed" },
    ];
    renderPage();
    expect((await card("ap-node")).queryByRole("note")).toBeNull();
    expect((await card("ap-exec")).queryByRole("note")).toBeNull();
  });

  it("says nothing about a tool approval that is no longer pending", async () => {
    server = [{ ...toolApproval, run_status: "succeeded", status: "approved", decided_at: "2026-10-15T11:00:00Z", decided_by: "api" }];
    renderPage("/approvals?status=all");
    expect((await card("ap-tool")).queryByRole("note")).toBeNull();
  });

  it("takes the note away when the approval turns out to have been decided already", async () => {
    server = [{ ...toolApproval, run_status: "succeeded" }];
    renderPage();
    const tool = await card("ap-tool");
    server[0].status = "approved";
    fireEvent.click(tool.getByRole("button", { name: "Approve" }));
    expect(await tool.findByText("This approval was already decided.")).not.toBeNull();
    expect(tool.queryByRole("note")).toBeNull();
  });
});
