// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import type { MergeRequest, Project, ProjectCheck, ProjectFact, Run } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { intake, mergeRequest, project, projectFact, run, workflow } from "@/test/fixtures";
import ProjectPage from "./ProjectPage";

let projects: Project[];
let facts: ProjectFact[];
let requests: MergeRequest[];
let runs: Run[];
let api: ReturnType<typeof installFakeApi>;

const SPEND = { day_usd: 1.2, month_usd: 12, day_until: "2026-10-18T00:00:00Z", month_until: "2026-11-01T00:00:00Z" };

beforeEach(() => {
  projects = [project({ description: "The playground repository", default_branch: "main", spend: SPEND })];
  facts = [];
  requests = [];
  runs = [];
  api = installFakeApi();
  api.on("GET", "/api/projects", () => projects);
  api.on("GET", "/api/projects/p1/facts", () => facts);
  api.on("GET", "/api/workflows", () => [workflow()]);
  api.on("GET", /^\/api\/merge-requests\?/, () => requests);
  // Like the real list: newest first, narrowed to the project when the query names one, and cut at `limit`.
  api.on("GET", /^\/api\/runs\?/, ({ url }) => {
    const query = new URL(url, "http://yuno.test").searchParams;
    const projectId = query.get("project_id");
    const limit = Number(query.get("limit") ?? 100);
    return runs.filter((r) => !projectId || r.project_id === projectId).slice(0, limit);
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function renderPage(path = "/projects/p1") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Where />
        <Routes>
          <Route path="/projects" element={<p>projects list</p>} />
          <Route path="/projects/:id" element={<ProjectPage />} />
          <Route path="/runs/:id" element={<p>run page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const where = () => screen.getByTestId("where").textContent;
const card = async (name: string) => {
  const heading = await screen.findByRole("heading", { name: new RegExp(`^${name}`) });
  return within(heading.closest("section") as HTMLElement);
};
const dialog = async () => within(await screen.findByRole("dialog"));

describe("the header", () => {
  it("names the project, its repository and its provider", async () => {
    renderPage();
    expect((await screen.findByRole("heading", { level: 1 })).textContent).toBe("yuno-playground");
    const repo = screen.getByRole("link", { name: "gitlab.com/andrebatista/yuno-playground.git" });
    expect(repo.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground.git");
    expect(screen.getByText("GitLab")).not.toBeNull();
    expect(screen.getByRole("link", { name: "← Projects" }).getAttribute("href")).toBe("/projects");
  });
});

describe("routes", () => {
  it("leads back to the list for an id that names no project, once the list is known", async () => {
    renderPage("/projects/gone");
    expect(await screen.findByText("projects list")).not.toBeNull();
    expect(where()).toBe("/projects");
  });

  it("waits for the list before deciding", async () => {
    renderPage();
    expect(screen.getByText("Loading project…")).not.toBeNull();
    expect(where()).toBe("/projects/p1");
    await screen.findByRole("heading", { level: 1 });
    expect(where()).toBe("/projects/p1");
  });

  it("says when the project cannot be loaded", async () => {
    api.on("GET", "/api/projects", () => apiError(500, "internal", "boom"));
    renderPage();
    expect((await screen.findByRole("alert")).textContent).toContain("Could not load the project: boom");
  });
});

describe("the Settings card", () => {
  it("shows the branch, the network and the description", async () => {
    renderPage();
    const settings = await card("Settings");
    expect(settings.getByText("main")).not.toBeNull();
    expect(settings.getByText("Network off")).not.toBeNull();
    expect(settings.getByText("The playground repository")).not.toBeNull();
  });

  it("says so when there is no description, and when the network is on", async () => {
    projects = [project({ description: "", sandbox_network: true, default_branch: "develop" })];
    renderPage();
    const settings = await card("Settings");
    expect(settings.getByText("No description.")).not.toBeNull();
    expect(settings.getByText("Network on")).not.toBeNull();
    expect(settings.getByText("develop")).not.toBeNull();
  });

  it("opens the form with Edit, over the page, and stays on the page when it closes", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const d = await dialog();
    expect(d.getByRole("heading", { name: "Edit yuno-playground" })).not.toBeNull();
    expect((d.getByLabelText(/^Repository URL/) as HTMLInputElement).value).toBe("https://gitlab.com/andrebatista/yuno-playground.git");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("yuno-playground");
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(where()).toBe("/projects/p1");
  });

  it("shows what was saved from the form once the project is read again", async () => {
    api.on("PUT", "/api/projects/p1", () => {
      projects = [project({ description: "The playground repository", instructions: "Use make.", spend: SPEND })];
      return projects[0];
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const d = await dialog();
    fireEvent.change(d.getByLabelText("Instructions"), { target: { value: "Use make." } });
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await (await card("Instructions")).findByText("Use make.")).not.toBeNull();
  });

  const checked = (over: Partial<ProjectCheck> = {}): ProjectCheck => ({
    ok: true,
    message: "Token accepted: default branch main, push allowed.",
    default_branch: "main",
    branch_exists: true,
    can_push: true,
    checked_at: "2026-10-16T10:00:00Z",
    ...over,
  });

  it("checks the token and shows the result line in green", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked());
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Check" }));
    const note = await screen.findByRole("status");
    expect(note.textContent).toBe("Token accepted: default branch main, push allowed.");
    expect(note.className).toContain("emerald");
    expect(api.made("POST", "/api/projects/p1/check")).toHaveLength(1);
  });

  it("shows a refusal in red, and a failed request too", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked({ ok: false, message: "The token was refused (401).", can_push: undefined, branch_exists: undefined }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Check" }));
    expect((await screen.findByRole("status")).className).toContain("red");
    cleanup();

    api.on("POST", "/api/projects/p1/check", () => apiError(500, "internal", "boom"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Check" }));
    expect((await screen.findByRole("status")).textContent).toBe("Could not check the project: boom");
  });

  it("does not show a result that predates the last change to the project", async () => {
    projects = [project({ updated_at: "2026-10-16T11:00:00Z" })];
    api.on("POST", "/api/projects/p1/check", () => checked({ checked_at: "2026-10-16T10:00:00Z" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Check" }));
    await waitFor(() => expect(api.made("POST", "/api/projects/p1/check")).toHaveLength(1));
    await waitFor(() => expect((screen.getByRole("button", { name: "Check" }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("asks before deleting, then removes the project and returns to the list", async () => {
    api.on("DELETE", "/api/projects/p1", () => {
      projects = [];
      return new Response(null, { status: 204 });
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    let d = await dialog();
    expect(d.getByRole("heading", { name: "Delete yuno-playground?" })).not.toBeNull();
    expect(d.getByText("Runs that used this project keep their history.")).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(api.made("DELETE", /projects/)).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete project" }));
    expect(await screen.findByText("projects list")).not.toBeNull();
    expect(api.made("DELETE", "/api/projects/p1")).toHaveLength(1);
    expect(where()).toBe("/projects");
  });

  it("shows why the API refused to delete", async () => {
    api.on("DELETE", "/api/projects/p1", () => apiError(404, "not_found", "project not found"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete project" }));
    expect((await d.findByRole("alert")).textContent).toContain("project not found");
    expect(where()).toBe("/projects/p1");
  });
});

describe("the Budget card", () => {
  it("shows today's and this month's spend over the caps, with when each window ends", async () => {
    projects = [project({ max_cost_usd_per_day: 5, max_cost_usd_per_month: 100, spend: SPEND })];
    renderPage();
    const budget = await card("Budget");
    expect(budget.getByText("$1.20 / $5.00")).not.toBeNull();
    expect(budget.getByText("$12.00 / $100.00")).not.toBeNull();
    expect(budget.getByText(`resets at ${formatDateTime("2026-10-18T00:00:00Z")}`)).not.toBeNull();
    expect(budget.getByText(`resets at ${formatDateTime("2026-11-01T00:00:00Z")}`)).not.toBeNull();
    expect(budget.getByText("Today")).not.toBeNull();
    expect(budget.getByText("This month")).not.toBeNull();
    expect(budget.queryByText("No cap set.")).toBeNull();
  });

  it("warns from 80 % and flags a reached cap in red, each window by its own cap", async () => {
    projects = [project({ max_cost_usd_per_day: 1.5, max_cost_usd_per_month: 12, spend: SPEND })];
    renderPage();
    const budget = await card("Budget");
    expect(budget.getByText("$1.20 / $1.50").className).toContain("amber");
    expect(budget.getByText("$12.00 / $12.00").className).toContain("red");
  });

  it("keeps the normal tone below 80 %", async () => {
    projects = [project({ max_cost_usd_per_day: 5, max_cost_usd_per_month: 100, spend: SPEND })];
    renderPage();
    const budget = await card("Budget");
    for (const text of ["$1.20 / $5.00", "$12.00 / $100.00"]) {
      expect(budget.getByText(text).className).not.toContain("amber");
      expect(budget.getByText(text).className).not.toContain("red");
    }
  });

  it("says No cap set. once, and shows the amounts alone, when there is no cap", async () => {
    renderPage();
    const budget = await card("Budget");
    expect(budget.getAllByText("No cap set.")).toHaveLength(1);
    expect(budget.getByText("$1.20")).not.toBeNull();
    expect(budget.getByText("$12.00")).not.toBeNull();
    expect(budget.queryByText(/resets at/)).toBeNull();
  });

  it("says which window has no cap when only one has", async () => {
    projects = [project({ max_cost_usd_per_month: 100, spend: SPEND })];
    renderPage();
    const budget = await card("Budget");
    expect(budget.getAllByText("No cap set.")).toHaveLength(1);
    expect(budget.getByText("$1.20").parentElement?.textContent).toContain("No cap set.");
    expect(budget.getByText("$12.00 / $100.00")).not.toBeNull();
  });

  it("shows nothing spent when the API sent no spend", async () => {
    projects = [project({ max_cost_usd_per_day: 5 })];
    renderPage();
    const budget = await card("Budget");
    expect(budget.getByText("$0.00 / $5.00")).not.toBeNull();
  });
});

describe("the Instructions card", () => {
  it("shows the text as written, line breaks kept", async () => {
    projects = [project({ instructions: "Run `make test` before you push.\nLeave vendor/ alone." })];
    renderPage();
    const instructions = await card("Instructions");
    const text = instructions.getByText(/Run `make test`/);
    expect(text.textContent).toBe("Run `make test` before you push.\nLeave vendor/ alone.");
    expect(text.className).toContain("whitespace-pre-wrap");
  });

  it("says so when there are none", async () => {
    renderPage();
    expect((await card("Instructions")).getByText("No instructions yet.")).not.toBeNull();
  });
});

describe("the Notes card", () => {
  it("shows each note's key, value, who saved it, in which run and when", async () => {
    facts = [
      projectFact({ id: "f1", key: "test-command", value: "go test ./...", agent_name: "Coder", source_run_id: "run77", updated_at: "2026-10-15T10:03:00Z" }),
      projectFact({ id: "f2", key: "layout", value: "cmd/ holds the binaries", agent_name: null, agent_id: undefined, source_run_id: undefined, updated_at: "2026-10-14T09:00:00Z", expires_at: "2027-01-12T09:00:00Z" }),
    ];
    renderPage();
    const notes = await card("Notes");
    const items = await notes.findAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText("test-command")).not.toBeNull();
    expect(within(items[0]).getByText("go test ./...")).not.toBeNull();
    expect(items[0].textContent).toContain("Saved by Coder");
    expect(items[0].textContent).toContain(formatDateTime("2026-10-15T10:03:00Z"));
    const runLink = within(items[0]).getByRole("link", { name: "Run run77" });
    expect(runLink.getAttribute("href")).toBe("/runs/run77");
    expect(items[1].textContent).toContain("Saved by an agent that was deleted");
    expect(items[1].textContent).toContain(`Expires ${formatDateTime("2027-01-12T09:00:00Z")}`);
    expect(within(items[1]).queryByRole("link")).toBeNull();
    expect(notes.getByText("2")).not.toBeNull();
  });

  it("says so when there are none", async () => {
    renderPage();
    expect(await (await card("Notes")).findByText("No notes yet. Agents save what they learn about the project while they work on it.")).not.toBeNull();
  });

  it("says when the notes cannot be loaded", async () => {
    api.on("GET", "/api/projects/p1/facts", () => apiError(500, "internal", "boom"));
    renderPage();
    expect((await (await card("Notes")).findByRole("alert")).textContent).toContain("Could not load the notes: boom");
  });

  it("deletes a note after a confirmation, and keeps the others", async () => {
    facts = [projectFact({ id: "f1", key: "test-command" }), projectFact({ id: "f2", key: "layout", value: "cmd/" })];
    api.on("DELETE", "/api/projects/p1/facts/f1", () => {
      facts = facts.filter((f) => f.id !== "f1");
      return new Response(null, { status: 204 });
    });
    renderPage();
    const notes = await card("Notes");
    fireEvent.click(await notes.findByRole("button", { name: "Delete note test-command" }));
    let d = await dialog();
    expect(d.getByRole("heading", { name: "Delete note test-command?" })).not.toBeNull();
    expect(d.getByText("Agents working on this project will no longer see it.")).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(api.made("DELETE", /facts/)).toHaveLength(0);

    fireEvent.click(notes.getByRole("button", { name: "Delete note test-command" }));
    d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete note" }));
    await waitFor(() => expect(notes.queryByText("test-command")).toBeNull());
    expect(api.made("DELETE", "/api/projects/p1/facts/f1")).toHaveLength(1);
    expect(notes.getByText("layout")).not.toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows why the API refused to delete a note", async () => {
    facts = [projectFact({})];
    api.on("DELETE", "/api/projects/p1/facts/f1", () => apiError(404, "not_found", "memory fact not found"));
    renderPage();
    fireEvent.click(await (await card("Notes")).findByRole("button", { name: "Delete note test-command" }));
    const d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete note" }));
    expect((await d.findByRole("alert")).textContent).toContain("memory fact not found");
  });
});

describe("the Merge requests table", () => {
  it("lists the project's requests with their number, title, run, state, pipeline, threads and update time", async () => {
    requests = [
      mergeRequest({ id: "mr2", number: 13, title: "Fix the build", run_id: "runb", state: "merged", ci_status: "passed", unresolved_threads: 0, updated_at: "2026-10-16T12:00:00Z" }),
      mergeRequest({ id: "mr1", number: 12, title: "Add the health endpoint", run_id: "runa", state: "open", ci_status: "pending", unresolved_threads: 3, updated_at: "2026-10-15T10:02:00Z" }),
      mergeRequest({ id: "mr3", number: 4, provider: "github", url: "https://github.com/o/r/pull/4", title: "Docs", run_id: "runc", state: "closed", ci_status: "failed", unresolved_threads: null }),
    ];
    renderPage();
    const section = await card("Merge requests");
    const headers = (await section.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual(["Request", "Title", "Runs", "State", "CI", "Unresolved threads", "Updated"]);
    const rows = section.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(3);

    const first = within(rows[0]);
    const number = first.getByRole("link", { name: "!13" });
    expect(number.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12");
    expect(number.getAttribute("target")).toBe("_blank");
    expect(first.getByText("Fix the build")).not.toBeNull();
    expect(first.getByRole("link", { name: "runb" }).getAttribute("href")).toBe("/runs/runb");
    expect(first.getByText("Merged")).not.toBeNull();
    expect(first.getByText("Passed")).not.toBeNull();
    expect(rows[0].querySelectorAll("td")[5].textContent).toBe("0");
    expect(first.getByText(formatDateTime("2026-10-16T12:00:00Z"))).not.toBeNull();

    const second = within(rows[1]);
    expect(second.getByText("Open")).not.toBeNull();
    expect(second.getByText("Pending").className).toContain("amber");
    expect(rows[1].querySelectorAll("td")[5].textContent).toBe("3");

    const third = within(rows[2]);
    expect(third.getByRole("link", { name: "#4" }).getAttribute("href")).toBe("https://github.com/o/r/pull/4");
    expect(third.getByText("Closed")).not.toBeNull();
    expect(third.getByText("Failed").className).toContain("red");
    expect(rows[2].querySelectorAll("td")[5].textContent).toBe("Unknown");
    expect(section.getByRole("heading", { name: /^Merge requests/ }).textContent).toBe("Merge requests3");
  });

  it("asks only for this project's requests", async () => {
    renderPage();
    await card("Merge requests");
    expect(api.made("GET", /^\/api\/merge-requests/).map((c) => c.url)).toEqual(["/api/merge-requests?project_id=p1"]);
  });

  it("says None yet. when there are none", async () => {
    renderPage();
    expect(await (await card("Merge requests")).findByText("None yet.")).not.toBeNull();
  });

  it("says when the requests cannot be loaded", async () => {
    api.on("GET", /^\/api\/merge-requests\?/, () => apiError(501, "not_implemented", "not available"));
    renderPage();
    expect((await (await card("Merge requests")).findByRole("alert")).textContent).toContain("Could not load the merge requests: not available");
  });
});

describe("the Runs column of the Merge requests table", () => {
  const runsCell = async (number: string) => {
    const section = await card("Merge requests");
    const row = (await section.findByRole("link", { name: number })).closest("tr") as HTMLElement;
    return within(row.querySelectorAll("td")[2]);
  };

  it("links the run that opened the request and counts the runs that updated it, linking to the runs list filtered by the request", async () => {
    requests = [
      mergeRequest({ id: "mr1", number: 12, run_id: "runa" }),
      mergeRequest({ id: "mr2", number: 13, run_id: "runb" }),
      mergeRequest({ id: "mr3", number: 14, run_id: "runc" }),
    ];
    runs = [
      run({ id: "u3", project_id: "p1", merge_request_id: "mr1" }),
      run({ id: "u2", project_id: "p1", merge_request_id: "mr1" }),
      run({ id: "u1", project_id: "p1", merge_request_id: "mr2" }),
      run({ id: "runa", project_id: "p1" }),
      run({ id: "unrelated", project_id: "p1", merge_request_id: "gone" }),
    ];
    renderPage();
    const first = await runsCell("!12");
    expect(first.getByRole("link", { name: "runa" }).getAttribute("href")).toBe("/runs/runa");
    const updates = await first.findByRole("link", { name: "+2 updates" });
    expect(updates.getAttribute("href")).toBe("/runs?merge_request_id=mr1");

    const second = await runsCell("!13");
    expect(second.getByRole("link", { name: "runb" })).not.toBeNull();
    expect(await second.findByRole("link", { name: "+1 update" })).not.toBeNull();
    expect(second.getByRole("link", { name: "+1 update" }).getAttribute("href")).toBe("/runs?merge_request_id=mr2");

    const third = await runsCell("!14");
    expect(third.getByRole("link", { name: "runc" })).not.toBeNull();
    expect(third.queryByRole("link", { name: /update/ })).toBeNull();
  });

  it("does not count the run that opened the request, and encodes the record's id in the link", async () => {
    requests = [mergeRequest({ id: "a/b", number: 12, run_id: "runa" })];
    runs = [run({ id: "runa", project_id: "p1", merge_request_id: "a/b" }), run({ id: "u1", project_id: "p1", merge_request_id: "a/b" })];
    renderPage();
    const cell = await runsCell("!12");
    expect((await cell.findByRole("link", { name: "+1 update" })).getAttribute("href")).toBe("/runs?merge_request_id=a%2Fb");
  });

  it("shows the opening run alone while the runs load or when they cannot be read", async () => {
    requests = [mergeRequest({ id: "mr1", number: 12, run_id: "runa" })];
    api.on("GET", /^\/api\/runs\?/, () => apiError(500, "internal", "boom"));
    renderPage();
    const cell = await runsCell("!12");
    expect(cell.getByRole("link", { name: "runa" })).not.toBeNull();
    expect(cell.queryByRole("link", { name: /update/ })).toBeNull();
    expect((await (await card("Recent runs")).findByRole("alert")).textContent).toContain("Could not load the runs: boom");
  });
});

describe("the Issue intake card", () => {
  const polledAt = () => new Date(Date.now() - 5 * 60_000).toISOString();

  it("sits beside the Budget card", async () => {
    renderPage();
    const budget = (await screen.findByRole("heading", { name: "Budget" })).closest("section") as HTMLElement;
    const intakeCard = (await screen.findByRole("heading", { name: "Issue intake" })).closest("section") as HTMLElement;
    expect(intakeCard.parentElement).toBe(budget.parentElement);
  });

  it("says Off with a disabled Poll now when the project's intake is off", async () => {
    projects = [project({ spend: SPEND, intake: intake({ enabled: false, workflow_id: null, workflow_name: null }) })];
    renderPage();
    const section = await card("Issue intake");
    expect(section.getByText("Off")).not.toBeNull();
    expect(section.getByText("Not polled yet")).not.toBeNull();
    expect((section.getByRole("button", { name: "Poll now" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("says On with the label and the workflow's name, when it last polled, and offers Poll now", async () => {
    projects = [project({ spend: SPEND, intake: intake({ label: "robot", workflow_name: "Dev pipeline (ours)", polled_at: polledAt() }) })];
    renderPage();
    const section = await card("Issue intake");
    expect(section.getByText("On: label robot, workflow Dev pipeline (ours)")).not.toBeNull();
    expect(section.getByText("Last poll 5 min ago")).not.toBeNull();
    expect((section.getByRole("button", { name: "Poll now" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("polls, shows what the pass did and reads the project and its runs again", async () => {
    projects = [project({ spend: SPEND, intake: intake({ polled_at: polledAt() }) })];
    runs = [run({ id: "run1", project_id: "p1", status: "running" })];
    api.on("POST", "/api/projects/p1/intake/poll", () => {
      projects = [project({ spend: SPEND, intake: intake({ polled_at: new Date().toISOString(), last_error: "intake: could not add the label" }) })];
      runs = [run({ id: "run2", project_id: "p1", status: "queued", trigger: "issue" }), ...runs];
      return { taken: 2, closed: 1 };
    });
    renderPage();
    const section = await card("Issue intake");
    await section.findByText("Last poll 5 min ago");
    const projectReads = api.made("GET", "/api/projects").length;
    const runReads = api.made("GET", /^\/api\/runs\?/).length;
    fireEvent.click(section.getByRole("button", { name: "Poll now" }));
    expect((await section.findByRole("status")).textContent).toBe("Took 2, closed 1");
    expect(api.made("POST", "/api/projects/p1/intake/poll")).toHaveLength(1);
    expect(api.made("POST", "/api/projects/p1/intake/poll")[0].body).toBeUndefined();
    expect(await section.findByText("Last poll just now")).not.toBeNull();
    expect(section.getByText("intake: could not add the label")).not.toBeNull();
    expect(api.made("GET", "/api/projects").length).toBeGreaterThan(projectReads);
    await waitFor(() => expect(api.made("GET", /^\/api\/runs\?/).length).toBeGreaterThan(runReads));
    expect(await (await card("Recent runs")).findByRole("link", { name: "run2" })).not.toBeNull();
  });

  it("shows the server's message when the intake turns out to be off, and reads the project again", async () => {
    projects = [project({ spend: SPEND, intake: intake() })];
    api.on("POST", "/api/projects/p1/intake/poll", () => {
      projects = [project({ spend: SPEND, intake: intake({ enabled: false }) })];
      return apiError(409, "intake_disabled", "issue intake is off for this project");
    });
    renderPage();
    const section = await card("Issue intake");
    await section.findByText(/^On: label/);
    fireEvent.click(section.getByRole("button", { name: "Poll now" }));
    expect((await section.findByRole("alert")).textContent).toBe("issue intake is off for this project");
    expect(await section.findByText("Off")).not.toBeNull();
    expect(section.queryByRole("status")).toBeNull();
  });
});

describe("the Recent runs card", () => {
  it("lists the project's runs, newest first, with status, time and a link", async () => {
    runs = [
      run({ id: "run1", project_id: "p1", status: "running", created_at: "2026-10-16T10:00:00Z" }),
      run({ id: "other", project_id: "p2", status: "failed" }),
      run({ id: "run2", project_id: "p1", status: "succeeded", created_at: "2026-10-15T10:00:00Z" }),
      run({ id: "bare", project_id: undefined, status: "queued" }),
    ];
    renderPage();
    const section = await card("Recent runs");
    const rows = (await section.findAllByRole("row")).slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole("link", { name: "run1" }).getAttribute("href")).toBe("/runs/run1");
    expect(within(rows[0]).getByText("running")).not.toBeNull();
    expect(within(rows[0]).getByText(formatDateTime("2026-10-16T10:00:00Z"))).not.toBeNull();
    expect(within(rows[1]).getByRole("link", { name: "run2" })).not.toBeNull();
    expect(within(rows[1]).getByText("succeeded")).not.toBeNull();
  });

  it("reads the project's 200 newest runs once, and shows the ten newest", async () => {
    runs = Array.from({ length: 15 }, (_, i) => run({ id: `run${String(i).padStart(2, "0")}`, project_id: "p1", status: "succeeded" }));
    renderPage();
    const section = await card("Recent runs");
    expect((await section.findAllByRole("row")).slice(1)).toHaveLength(10);
    expect(section.getByRole("link", { name: "run09" })).not.toBeNull();
    expect(section.queryByRole("link", { name: "run10" })).toBeNull();
    // the Merge requests table counts from the same read
    await waitFor(() => expect(api.made("GET", /^\/api\/merge-requests/)).toHaveLength(1));
    expect(api.made("GET", /^\/api\/runs/).map((c) => c.url)).toEqual(["/api/runs?project_id=p1&limit=200"]);
  });

  it("finds the project's runs however many newer runs other projects have", async () => {
    runs = [
      ...Array.from({ length: 250 }, (_, i) => run({ id: `busy${i}`, project_id: "p2", status: "succeeded" })),
      run({ id: "old1", project_id: "p1", status: "failed", created_at: "2026-09-01T10:00:00Z" }),
      run({ id: "old2", project_id: "p1", status: "succeeded", created_at: "2026-08-30T10:00:00Z" }),
    ];
    renderPage();
    const section = await card("Recent runs");
    expect((await section.findAllByRole("row")).slice(1)).toHaveLength(2);
    expect(section.getByRole("link", { name: "old1" })).not.toBeNull();
    expect(section.getByRole("link", { name: "old2" })).not.toBeNull();
    expect(api.made("GET", /^\/api\/runs/)).toHaveLength(1);
  });

  it("says so when the project has no runs", async () => {
    runs = [run({ id: "other", project_id: "p2" })];
    renderPage();
    expect(await (await card("Recent runs")).findByText("No runs yet.")).not.toBeNull();
  });

  it("says when the runs cannot be loaded", async () => {
    api.on("GET", /^\/api\/runs\?/, () => apiError(500, "internal", "boom"));
    renderPage();
    expect((await (await card("Recent runs")).findByRole("alert")).textContent).toContain("Could not load the runs: boom");
  });
});
