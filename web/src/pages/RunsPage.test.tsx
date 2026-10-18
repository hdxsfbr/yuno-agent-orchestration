// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation, useParams } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Run } from "@/lib/api";
import { installFakeApi } from "@/test/fakeApi";
import { mergeRequest, projectSettings } from "@/test/fixtures";
import RunsPage from "./RunsPage";

const DONE = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";
const QUEUED = "01a14162-0000-7000-8000-000000000001";
const DELEGATED = "01a14163-0000-7000-8000-000000000002";
const DELEGATED_BARE = "01a14164-0000-7000-8000-000000000003";
const OTHER_REF = "01a14165-0000-7000-8000-000000000004";
const ODD_ID = "01a14166-0000-7000-8000-000000000005";
const PROJECT_RUN = "01a14167-0000-7000-8000-000000000006";
const SNAPSHOT_ONLY = "01a14168-0000-7000-8000-000000000007";

const base = (over: Partial<Run>): Run => ({
  id: DONE,
  workflow_id: "wf",
  workflow_version: 1,
  trigger: "manual",
  status: "succeeded",
  input: {},
  state: {},
  limits: {},
  step_count: 6,
  tool_call_count: 6,
  cost_usd_micros: 62_064,
  created_at: "2026-10-15T21:04:53Z",
  started_at: "2026-10-15T21:04:53Z",
  finished_at: "2026-10-15T21:05:18Z",
  ...over,
});

const runs: Run[] = [
  base({ agent_names: ["Coder"], models: ["claude-sonnet-5-5"], toolchain: "go" }),
  base({ id: QUEUED, status: "queued", step_count: 0, tool_call_count: 0, cost_usd_micros: 0, started_at: undefined, finished_at: undefined }),
  base({ id: DELEGATED, trigger: "channel", trigger_ref: "conversation:c1", agent_names: ["Reviewer"] }),
  base({ id: DELEGATED_BARE, trigger: "channel", trigger_ref: "" }),
  base({ id: OTHER_REF, trigger: "channel", trigger_ref: "telegram:1001" }),
  base({ id: ODD_ID, trigger: "channel", trigger_ref: "conversation:a/b c" }),
  base({
    id: PROJECT_RUN,
    project_id: "p1",
    project: { name: "yuno-playground", repo_url: "https://gitlab.com/g/p.git", provider: "gitlab", default_branch: "main", sandbox_network: false },
  }),
  base({
    id: SNAPSHOT_ONLY,
    project: { name: "old-name", repo_url: "https://gitlab.com/g/o.git", provider: "gitlab", default_branch: "main", sandbox_network: false },
  }),
];

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url.startsWith("/api/runs?")) return json(runs);
      if (url === "/api/events") {
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            c.enqueue(new TextEncoder().encode(": connected\n\n"));
            init?.signal?.addEventListener("abort", () => {
              try {
                c.close();
              } catch {
                /* already closed */
              }
            });
          },
        });
        return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
      }
      return json({ error: { code: "not_found", message: "not found" } }, 404);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function RunMarker() {
  const { id } = useParams();
  return <p>run page {id}</p>;
}

function ProjectMarker() {
  const { id } = useParams();
  return <p>project page {id}</p>;
}

function ConversationMarker() {
  const { conversationId } = useParams();
  return <p>conversation page {conversationId}</p>;
}

function Where() {
  const { pathname, search } = useLocation();
  return <p data-testid="where">{pathname + search}</p>;
}

function renderPage(path = "/runs") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Where />
        <Routes>
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunMarker />} />
          <Route path="/projects/:id" element={<ProjectMarker />} />
          <Route path="/channels/:conversationId" element={<ConversationMarker />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunsPage", () => {
  it("lists each run with its agent and model, or a dash before it has either", async () => {
    renderPage();
    const link = await screen.findByTitle(DONE);
    const row = link.closest("tr")!;
    expect(screen.getByRole("columnheader", { name: "Agent" })).not.toBeNull();
    expect(screen.getByRole("columnheader", { name: "Model" })).not.toBeNull();
    expect(within(row).getByText("Coder")).not.toBeNull();
    expect(within(row).getByText("claude-sonnet-5-5")).not.toBeNull();

    const queued = within(screen.getByTitle(QUEUED).closest("tr")!).getAllByRole("cell");
    expect(queued[1].textContent).toBe("—");
    expect(queued[2].textContent).toBe("—");
  });

  it("shows the toolchain of a project run next to its status, and nothing for the others", async () => {
    renderPage();
    const row = (await screen.findByTitle(DONE)).closest("tr")!;
    expect(within(row).getByText("go").getAttribute("title")).toContain("Toolchain");
    const queued = screen.getByTitle(QUEUED).closest("tr")!;
    expect(within(queued).queryByTitle(/Toolchain/)).toBeNull();
  });

  it("names the project of a run, linking to its page, and shows a dash for a run without one", async () => {
    renderPage();
    const column = (await screen.findAllByRole("columnheader")).findIndex((h) => h.textContent === "Project");
    expect(column).toBeGreaterThan(-1);
    const cell = (id: string) => within(screen.getByTitle(id).closest("tr")!).getAllByRole("cell")[column];

    const link = within(cell(PROJECT_RUN)).getByRole("link", { name: "yuno-playground" });
    expect(link.getAttribute("href")).toBe("/projects/p1");
    expect(cell(DONE).textContent).toBe("—");
    expect(cell(QUEUED).textContent).toBe("—");
  });

  it("shows the snapshot's name without a link when the run has no project id", async () => {
    renderPage();
    const cell = within((await screen.findByTitle(SNAPSHOT_ONLY)).closest("tr")!).getByText("old-name").closest("td")!;
    expect(cell.textContent).toBe("old-name");
    expect(within(cell).queryByRole("link")).toBeNull();
  });

  it("opens the project, not the run, when the project's link is clicked", async () => {
    renderPage();
    const row = (await screen.findByTitle(PROJECT_RUN)).closest("tr")!;
    fireEvent.click(within(row).getByRole("link", { name: "yuno-playground" }));
    expect(await screen.findByText("project page p1")).not.toBeNull();
    expect(screen.queryByText(`run page ${PROJECT_RUN}`)).toBeNull();
  });

  it("opens the run when any cell of its row is clicked", async () => {
    renderPage();
    const row = (await screen.findByTitle(DONE)).closest("tr")!;
    fireEvent.click(within(row).getByText("manual"));
    expect(await screen.findByText(`run page ${DONE}`)).not.toBeNull();
  });
});

describe("RunsPage delegated runs", () => {
  const triggerCell = async (id: string) => within((await screen.findByTitle(id)).closest("tr")!).getAllByRole("cell")[5];

  it("shows a run an agent delegated from a conversation as Delegated, with a link back to that conversation", async () => {
    renderPage();
    const cell = within(await triggerCell(DELEGATED));
    expect(cell.getByText("Delegated")).not.toBeNull();
    expect(cell.queryByText("channel")).toBeNull();
    expect(cell.getByRole("link", { name: "Conversation" }).getAttribute("href")).toBe("/channels/c1");
  });

  it("opens the conversation, not the run, when its link is clicked", async () => {
    renderPage();
    fireEvent.click(within(await triggerCell(DELEGATED)).getByRole("link", { name: "Conversation" }));
    expect(await screen.findByText("conversation page c1")).not.toBeNull();
    expect(screen.queryByText(`run page ${DELEGATED}`)).toBeNull();
  });

  it("still opens the run from the rest of the row", async () => {
    renderPage();
    const row = (await screen.findByTitle(DELEGATED)).closest("tr")!;
    fireEvent.click(within(row).getByText("Delegated"));
    expect(await screen.findByText(`run page ${DELEGATED}`)).not.toBeNull();
  });

  it("shows Delegated without a link when the run has no conversation", async () => {
    renderPage();
    const cell = within(await triggerCell(DELEGATED_BARE));
    expect(cell.getByText("Delegated")).not.toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });

  it("shows a channel run whose reference names no conversation as the trigger with its reference, not as delegated", async () => {
    renderPage();
    const cell = within(await triggerCell(OTHER_REF));
    expect(cell.getByText("channel")).not.toBeNull();
    const ref = cell.getByText("telegram:1001");
    expect(ref.className).toContain("font-mono");
    expect(cell.queryByText("Delegated")).toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });

  it("encodes the conversation id in the link", async () => {
    renderPage();
    const cell = within(await triggerCell(ODD_ID));
    expect(cell.getByRole("link", { name: "Conversation" }).getAttribute("href")).toBe("/channels/a%2Fb%20c");
  });

  it("leaves other triggers as they are", async () => {
    renderPage();
    const cell = within(await triggerCell(DONE));
    expect(cell.getByText("manual")).not.toBeNull();
    expect(cell.queryByText("Delegated")).toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });
});

describe("RunsPage live list", () => {
  it("refreshes the list on any event, once for a burst", async () => {
    const api = installFakeApi();
    api.on("GET", /^\/api\/runs\?/, () => runs);
    renderPage();
    await screen.findByTitle(DONE);
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));

    for (const event of ["log", "tool_call", "model_call", "agent_message"]) api.send("/api/events", event, {});
    await waitFor(() => expect(api.made("GET", /^\/api\/runs\?/)).toHaveLength(2));
    await act(async () => void (await new Promise((r) => setTimeout(r, 800))));
    expect(api.made("GET", /^\/api\/runs\?/)).toHaveLength(2);
  });
});

describe("RunsPage trigger filter", () => {
  let api: ReturnType<typeof installFakeApi>;
  let listed: Run[];
  const runUrls = () => api.made("GET", /^\/api\/runs\?/).map((c) => c.url);
  const triggerSelect = () => screen.getByLabelText("Trigger") as HTMLSelectElement;
  const where = () => screen.getByTestId("where").textContent;

  beforeEach(() => {
    listed = [base({ agent_names: ["Coder"] })];
    api = installFakeApi();
    api.on("GET", /^\/api\/runs\?/, () => listed);
  });

  it("sits next to the status filter with All and the five triggers, All chosen", async () => {
    renderPage();
    await screen.findByTitle(DONE);
    const select = triggerSelect();
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["All", "Manual", "Schedule", "Channel", "Eval", "Issue"]);
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "manual", "schedule", "channel", "eval", "issue"]);
    expect(select.value).toBe("");
    expect(screen.getByLabelText("Status").parentElement?.parentElement).toBe(select.parentElement?.parentElement);
    expect(runUrls()).toEqual(["/api/runs?limit=100"]);
  });

  it("asks the server for the chosen trigger, keeps it in the address and drops it again with All", async () => {
    renderPage();
    await screen.findByTitle(DONE);
    fireEvent.change(triggerSelect(), { target: { value: "issue" } });
    await waitFor(() => expect(runUrls()).toContain("/api/runs?trigger=issue&limit=100"));
    expect(where()).toBe("/runs?trigger=issue");
    fireEvent.change(triggerSelect(), { target: { value: "" } });
    await waitFor(() => expect(where()).toBe("/runs"));
    expect(runUrls().at(-1)).toBe("/api/runs?limit=100");
  });

  it("reads the trigger and the status from the address, and changes one without losing the other", async () => {
    renderPage("/runs?status=failed&trigger=schedule");
    await screen.findByTitle(DONE);
    expect(triggerSelect().value).toBe("schedule");
    expect((screen.getByLabelText("Status") as HTMLSelectElement).value).toBe("failed");
    expect(runUrls()).toEqual(["/api/runs?status=failed&trigger=schedule&limit=100"]);
    fireEvent.change(triggerSelect(), { target: { value: "eval" } });
    await waitFor(() => expect(runUrls()).toContain("/api/runs?status=failed&trigger=eval&limit=100"));
    expect(where()).toBe("/runs?status=failed&trigger=eval");
    fireEvent.change(screen.getByLabelText("Status"), { target: { value: "" } });
    await waitFor(() => expect(where()).toBe("/runs?trigger=eval"));
  });

  it("ignores a trigger in the address that is not one of the five", async () => {
    renderPage("/runs?trigger=webhook");
    await screen.findByTitle(DONE);
    expect(triggerSelect().value).toBe("");
    expect(runUrls()).toEqual(["/api/runs?limit=100"]);
  });

  it("says no run matches when a filtered list is empty", async () => {
    listed = [];
    renderPage("/runs?trigger=issue");
    expect(await screen.findByText("No runs match.")).not.toBeNull();
  });
});

describe("RunsPage issue runs", () => {
  const ISSUE = "01a14170-0000-7000-8000-000000000001";
  const ISSUE_GITHUB = "01a14170-0000-7000-8000-000000000002";
  const ISSUE_BARE = "01a14170-0000-7000-8000-000000000003";
  const ISSUE_ODD = "01a14170-0000-7000-8000-000000000004";
  let api: ReturnType<typeof installFakeApi>;
  const triggerCell = async (id: string) => within((await screen.findByTitle(id)).closest("tr")!).getAllByRole("cell")[5];

  beforeEach(() => {
    api = installFakeApi();
    api.on("GET", /^\/api\/runs\?/, () => [
      base({ id: ISSUE, trigger: "issue", trigger_ref: "p1#7", project_id: "p1", project: projectSettings() }),
      base({ id: ISSUE_GITHUB, trigger: "issue", trigger_ref: "p2#31", project_id: "p2", project: projectSettings({ name: "docs-site", provider: "github", repo_url: "https://github.com/acme/docs.git" }) }),
      base({ id: ISSUE_BARE, trigger: "issue", trigger_ref: "p9#4" }),
      base({ id: ISSUE_ODD, trigger: "issue", trigger_ref: "p1", project_id: "p1", project: projectSettings() }),
    ]);
  });

  it("shows issue #n linking to the issue on GitLab, and not the raw reference", async () => {
    renderPage();
    const cell = within(await triggerCell(ISSUE));
    const link = cell.getByRole("link", { name: "issue #7" });
    expect(link.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/issues/7");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(cell.queryByText("p1#7")).toBeNull();
    expect(cell.queryByText("issue")).toBeNull();
  });

  it("links to the issue on GitHub", async () => {
    renderPage();
    expect(within(await triggerCell(ISSUE_GITHUB)).getByRole("link", { name: "issue #31" }).getAttribute("href")).toBe("https://github.com/acme/docs/issues/31");
  });

  it("shows issue #n without a link when the run kept no snapshot of its project", async () => {
    renderPage();
    const cell = within(await triggerCell(ISSUE_BARE));
    expect(cell.getByText("issue #4")).not.toBeNull();
    expect(cell.queryByRole("link")).toBeNull();
  });

  it("shows the trigger with its reference as it is when the reference carries no issue number", async () => {
    renderPage();
    const cell = await triggerCell(ISSUE_ODD);
    expect(cell.textContent).toBe("issue");
    expect(within(cell).queryByRole("link")).toBeNull();
  });

  it("opens the issue, not the run, when the link is clicked", async () => {
    renderPage();
    fireEvent.click(within(await triggerCell(ISSUE)).getByRole("link", { name: "issue #7" }));
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText(`run page ${ISSUE}`)).toBeNull();
  });
});

describe("RunsPage runs that update a merge request", () => {
  const UPDATING = "01a14180-0000-7000-8000-000000000001";
  const UPDATING_GITHUB = "01a14180-0000-7000-8000-000000000002";
  const OPENED = "01a14180-0000-7000-8000-000000000003";
  const PLAIN = "01a14180-0000-7000-8000-000000000004";
  let api: ReturnType<typeof installFakeApi>;
  let listed: Run[];
  const projectCell = async (id: string) => within((await screen.findByTitle(id)).closest("tr")!).getAllByRole("cell")[3];
  const where = () => screen.getByTestId("where").textContent;

  beforeEach(() => {
    listed = [
      base({ id: UPDATING, project_id: "p1", project: projectSettings(), merge_request_id: "mr1", merge_request: mergeRequest({ id: "mr1", run_id: OPENED }) }),
      base({
        id: UPDATING_GITHUB,
        project_id: "p2",
        project: projectSettings({ name: "docs-site", provider: "github" }),
        merge_request_id: "mr2",
        merge_request: mergeRequest({ id: "mr2", provider: "github", number: 4, url: "https://github.com/acme/docs/pull/4" }),
      }),
      base({ id: OPENED, project_id: "p1", project: projectSettings(), merge_request: mergeRequest({ id: "mr1", run_id: OPENED }) }),
      base({ id: PLAIN, project_id: "p1", project: projectSettings() }),
    ];
    api = installFakeApi();
    api.on("GET", /^\/api\/runs\?/, () => listed);
  });

  it("shows !12 after the project's name, linking to the request on the provider", async () => {
    renderPage();
    const cell = await projectCell(UPDATING);
    expect(cell.textContent).toBe("yuno-playground!12");
    const mark = within(cell).getByRole("link", { name: "!12" });
    expect(mark.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12");
    expect(mark.getAttribute("title")).toBe("Updates merge request !12");
    expect(within(cell).getByRole("link", { name: "yuno-playground" }).getAttribute("href")).toBe("/projects/p1");
  });

  it("writes #4 on GitHub", async () => {
    renderPage();
    const mark = within(await projectCell(UPDATING_GITHUB)).getByRole("link", { name: "#4" });
    expect(mark.getAttribute("title")).toBe("Updates pull request #4");
  });

  it("marks neither the run that opened a request nor a run that names none", async () => {
    renderPage();
    expect((await projectCell(OPENED)).textContent).toBe("yuno-playground");
    expect((await projectCell(PLAIN)).textContent).toBe("yuno-playground");
  });

  it("lists the runs that named a request when the address says so, and says which request", async () => {
    renderPage("/runs?merge_request_id=mr1");
    await screen.findByTitle(UPDATING);
    expect(api.made("GET", /^\/api\/runs\?/).map((c) => c.url)).toEqual(["/api/runs?merge_request_id=mr1&limit=100"]);
    expect(screen.getByText("Merge request")).not.toBeNull();
    expect(screen.getByText("!12", { selector: "span" })).not.toBeNull();
  });

  it("goes back to every run with the Clear merge request filter button", async () => {
    renderPage("/runs?status=failed&merge_request_id=mr1");
    await screen.findByTitle(UPDATING);
    fireEvent.click(screen.getByRole("button", { name: "Clear merge request filter" }));
    await waitFor(() => expect(where()).toBe("/runs?status=failed"));
    expect(screen.queryByRole("button", { name: "Clear merge request filter" })).toBeNull();
    expect(api.made("GET", /^\/api\/runs\?/).map((c) => c.url).at(-1)).toBe("/api/runs?status=failed&limit=100");
  });

  it("names the request by the end of its id until a row tells its number, and says no run matches when the list is empty", async () => {
    listed = [];
    renderPage("/runs?merge_request_id=0192-abcdef12");
    expect(await screen.findByText("No runs match.")).not.toBeNull();
    expect(screen.getByText("abcdef12")).not.toBeNull();
  });
});
