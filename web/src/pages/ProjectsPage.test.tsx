// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import type { Project, ProjectCheck, ProjectInput } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import ProjectsPage from "./ProjectsPage";

const RUN = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";
const TOKEN = "glpat-not-a-real-token-00";

let projects: Project[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  projects = [
    project({ last_run: { id: RUN, status: "succeeded", toolchain: "go", created_at: "2026-10-16T09:00:00Z", finished_at: "2026-10-16T09:05:00Z" } }),
    project({
      id: "p2",
      name: "docs-site",
      repo_url: "https://github.com/acme/docs",
      provider: "github",
      default_branch: "develop",
      sandbox_network: true,
      description: "Public documentation",
    }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/projects", () => projects);
  // The project form lists the workflows the issue intake can run.
  api.on("GET", "/api/workflows", () => []);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function renderPage(path = "/projects") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Where />
        <Routes>
          <Route path="/projects" element={<ProjectsPage />} />
          <Route path="/projects/:id" element={<p>project page</p>} />
          <Route path="/projects/:id/edit" element={<ProjectsPage />} />
          <Route path="/runs/:id" element={<p>run page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const dialog = async () => within(await screen.findByRole("dialog"));
const rowOf = async (name: string) => (await screen.findByRole("link", { name })).closest("tr") as HTMLElement;
const where = () => screen.getByTestId("where").textContent;
const openNew = async () => {
  fireEvent.click(await screen.findByRole("button", { name: "New project" }));
  return dialog();
};
const fill = (d: ReturnType<typeof within>, label: RegExp | string, value: string) => fireEvent.change(d.getByLabelText(label), { target: { value } });

describe("ProjectsPage list", () => {
  it("shows each project's name, repository, provider, branch, last run and network", async () => {
    renderPage();
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual(["Name", "Repository", "Provider", "Default branch", "Last run", "Spend", "Sandbox network", "Actions"]);

    const first = within(await rowOf("yuno-playground"));
    const cells = first.getAllByRole("cell");
    expect(cells[1].textContent).toBe("gitlab.com/andrebatista/yuno-playground.git");
    expect(cells[1].getAttribute("title")).toBe("https://gitlab.com/andrebatista/yuno-playground.git");
    expect(cells[2].textContent).toBe("GitLab");
    expect(cells[3].textContent).toBe("main");
    expect(first.getByText("Go").getAttribute("title")).toContain("Toolchain");
    expect(first.getByText("succeeded")).not.toBeNull();
    expect(first.getByText("Network off")).not.toBeNull();

    const second = within(await rowOf("docs-site"));
    const other = second.getAllByRole("cell");
    expect(other[0].textContent).toContain("Public documentation");
    expect(other[1].textContent).toBe("github.com/acme/docs");
    expect(other[2].textContent).toBe("GitHub");
    expect(other[3].textContent).toBe("develop");
    expect(other[4].textContent).toBe("—");
    expect(second.getByText("Network on")).not.toBeNull();
  });

  it("links the last run's status to that run", async () => {
    renderPage();
    const row = within(await rowOf("yuno-playground"));
    fireEvent.click(row.getByRole("link", { name: "succeeded" }));
    expect(await screen.findByText("run page")).not.toBeNull();
    expect(where()).toBe(`/runs/${RUN}`);
  });

  it("shows the run's status alone when its checkout has no toolchain yet", async () => {
    projects = [project({ last_run: { id: RUN, status: "queued", created_at: "2026-10-16T09:00:00Z" } })];
    renderPage();
    const cell = (await rowOf("yuno-playground")).querySelectorAll("td")[4];
    expect(cell.textContent).toBe("queued");
  });

  it("says so when there are none", async () => {
    projects = [];
    renderPage();
    expect(await screen.findByText(/No projects yet/)).not.toBeNull();
    expect(screen.getByRole("button", { name: "New project" })).not.toBeNull();
  });

  it("says when the list cannot be loaded", async () => {
    api.on("GET", "/api/projects", () => apiError(500, "internal", "boom"));
    renderPage();
    expect((await screen.findByRole("alert")).textContent).toContain("Could not load projects: boom");
  });
});

describe("creating a project", () => {
  it("sends the fields and the token, with the provider left to detection", async () => {
    api.on("POST", "/api/projects", (r) => project({ id: "p9", ...(r.body as ProjectInput), provider: "gitlab", default_branch: "main" } as Partial<Project>));
    renderPage();
    const d = await openNew();

    const token = d.getByLabelText(/^Access token/) as HTMLInputElement;
    expect(token.type).toBe("password");
    expect(token.getAttribute("autocomplete")).toBe("new-password");
    expect(token.value).toBe("");
    expect((d.getByLabelText("Provider") as HTMLSelectElement).value).toBe("");
    expect(d.getByRole("option", { name: "Detect from the URL" })).not.toBeNull();
    expect((d.getByLabelText("Default branch") as HTMLInputElement).placeholder).toBe("main");
    expect(d.getByText(/Lets agents install dependencies/)).not.toBeNull();
    expect(d.getByText(/only for repositories you trust/)).not.toBeNull();

    fill(d, /^Name/, " api-server ");
    fill(d, /^Repository URL/, " https://gitlab.com/acme/api ");
    fill(d, /^Access token/, TOKEN);
    fill(d, "Description", "The public API");
    fireEvent.click(d.getByRole("checkbox", { name: /Allow network access/ }));
    fireEvent.click(d.getByRole("button", { name: "Create project" }));

    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(api.made("POST", "/api/projects")[0].body).toEqual({
      name: "api-server",
      repo_url: "https://gitlab.com/acme/api",
      provider: "",
      sandbox_network: true,
      description: "The public API",
      instructions: "",
      max_cost_usd_per_day: null,
      max_cost_usd_per_month: null,
      token: TOKEN,
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("sends the provider and branch that were chosen", async () => {
    api.on("POST", "/api/projects", () => project({ id: "p9" }));
    renderPage();
    const d = await openNew();
    fill(d, /^Name/, "mirror");
    fill(d, /^Repository URL/, "https://git.example.com/acme/mirror");
    fill(d, "Provider", "github");
    fill(d, "Default branch", "develop");
    fill(d, /^Access token/, TOKEN);
    fireEvent.click(d.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(api.made("POST", "/api/projects")[0].body).toMatchObject({ provider: "github", default_branch: "develop", sandbox_network: false });
  });

  it("asks for a name, a repository and a token before sending", async () => {
    renderPage();
    const d = await openNew();
    fireEvent.click(d.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(d.getByLabelText(/^Name/).getAttribute("aria-invalid")).toBe("true"));
    expect(d.getByLabelText(/^Name/).parentElement?.textContent).toContain("Name is required");
    expect(d.getByLabelText(/^Repository URL/).parentElement?.textContent).toContain("Repository URL is required");
    expect(d.getByLabelText(/^Access token/).parentElement?.textContent).toContain("A token is required");
    expect(api.made("POST", "/api/projects")).toHaveLength(0);
  });

  it("puts the API's 422 next to the field it names", async () => {
    api.on("POST", "/api/projects", () =>
      apiError(422, "validation_failed", "validation failed", [
        { path: "name", message: 'a project named "yuno-playground" already exists' },
        { path: "repo_url", message: "must be an https URL" },
        { path: "token", message: "is required" },
      ]),
    );
    renderPage();
    const d = await openNew();
    fill(d, /^Name/, "yuno-playground");
    fill(d, /^Repository URL/, "http://gitlab.com/g/p");
    fill(d, /^Access token/, TOKEN);
    fireEvent.click(d.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(d.getByLabelText(/^Name/).parentElement?.textContent).toContain("already exists"));
    expect(d.getByLabelText(/^Repository URL/).parentElement?.textContent).toContain("must be an https URL");
    expect(d.getByLabelText(/^Access token/).parentElement?.textContent).toContain("is required");
    expect(d.getByLabelText("Default branch").getAttribute("aria-invalid")).toBeNull();
    expect(screen.getByRole("dialog")).not.toBeNull();
  });

  it("shows why the API refused when it names no field", async () => {
    api.on("POST", "/api/projects", () => apiError(503, "project_tokens_unavailable", "no token can be stored"));
    renderPage();
    const d = await openNew();
    fill(d, /^Name/, "api-server");
    fill(d, /^Repository URL/, "https://gitlab.com/acme/api");
    fill(d, /^Access token/, TOKEN);
    fireEvent.click(d.getByRole("button", { name: "Create project" }));
    expect((await d.findByRole("alert")).textContent).toBe("no token can be stored");
  });
});

describe("editing a project", () => {
  const edit = async (name = "yuno-playground") => {
    fireEvent.click(await screen.findByRole("link", { name: `Edit ${name}` }));
    return dialog();
  };

  it("opens on the saved settings with an empty token and the hint, and leaves the token out when it stays empty", async () => {
    api.on("PUT", /^\/api\/projects\/([^/]+)$/, (r) => project({ ...(r.body as Partial<Project>) }));
    renderPage();
    const d = await edit();
    expect(d.getByRole("heading", { name: "Edit yuno-playground" })).not.toBeNull();
    expect((d.getByLabelText(/^Name/) as HTMLInputElement).value).toBe("yuno-playground");
    expect((d.getByLabelText(/^Repository URL/) as HTMLInputElement).value).toBe("https://gitlab.com/andrebatista/yuno-playground.git");
    expect((d.getByLabelText("Provider") as HTMLSelectElement).value).toBe("gitlab");
    expect((d.getByLabelText("Default branch") as HTMLInputElement).value).toBe("main");
    const token = d.getByLabelText(/^Access token/) as HTMLInputElement;
    expect(token.value).toBe("");
    expect(d.getByText("A token is set. Enter a new one to replace it.")).not.toBeNull();
    expect(d.queryByText(/not shown again/)).toBeNull();

    fill(d, "Description", "Playground repository");
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    const body = api.made("PUT", "/api/projects/p1")[0].body as Record<string, unknown>;
    expect("token" in body).toBe(false);
    expect(body).toEqual({
      name: "yuno-playground",
      repo_url: "https://gitlab.com/andrebatista/yuno-playground.git",
      provider: "gitlab",
      default_branch: "main",
      sandbox_network: false,
      description: "Playground repository",
      instructions: "",
      max_cost_usd_per_day: null,
      max_cost_usd_per_month: null,
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(where()).toBe("/projects");
  });

  it("sends a new token when one is entered", async () => {
    api.on("PUT", /^\/api\/projects\/([^/]+)$/, () => project({}));
    renderPage();
    const d = await edit();
    fill(d, /^Access token/, `  ${TOKEN}  `);
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect((api.made("PUT", "/api/projects/p1")[0].body as ProjectInput).token).toBe(TOKEN);
  });

  it("does not ask for a token on an edit", async () => {
    renderPage();
    const d = await edit();
    expect(d.getByLabelText(/^Access token/).parentElement?.textContent).not.toContain("*");
    fill(d, /^Name/, "");
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(d.getByLabelText(/^Name/).getAttribute("aria-invalid")).toBe("true"));
    expect(d.getByLabelText(/^Access token/).getAttribute("aria-invalid")).toBeNull();
    expect(api.made("PUT", /./)).toHaveLength(0);
  });

  it("opens from Edit and closes back to the list", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "Edit docs-site" });
    expect(link.getAttribute("href")).toBe("/projects/p2/edit");
    fireEvent.click(link);
    const d = await dialog();
    expect(d.getByRole("heading", { name: "Edit docs-site" })).not.toBeNull();
    expect(where()).toBe("/projects/p2/edit");
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(where()).toBe("/projects");
  });
});

describe("routes", () => {
  it("opens the form of the project /projects/<id>/edit names", async () => {
    renderPage("/projects/p2/edit");
    const d = await dialog();
    expect(d.getByRole("heading", { name: "Edit docs-site" })).not.toBeNull();
    expect((d.getByLabelText(/^Repository URL/) as HTMLInputElement).value).toBe("https://github.com/acme/docs");
    expect(screen.getByRole("link", { name: "yuno-playground" })).not.toBeNull();
  });

  it("falls back to the list when the id names no project", async () => {
    renderPage("/projects/gone/edit");
    expect(await screen.findByRole("link", { name: "yuno-playground" })).not.toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(where()).toBe("/projects");
  });

  it("waits for the list before deciding the id is unknown", async () => {
    renderPage("/projects/p1/edit");
    expect(where()).toBe("/projects/p1/edit");
    expect((await dialog()).getByRole("heading", { name: "Edit yuno-playground" })).not.toBeNull();
    expect(where()).toBe("/projects/p1/edit");
  });
});

describe("checking a project", () => {
  const checked = (over: Partial<ProjectCheck> = {}): ProjectCheck => ({
    ok: true,
    message: "Token accepted: default branch main, push allowed.",
    default_branch: "main",
    branch_exists: true,
    can_push: true,
    checked_at: "2026-10-16T10:00:00Z",
    ...over,
  });
  const check = async (name = "yuno-playground") => {
    fireEvent.click(await screen.findByRole("button", { name: `Check ${name}` }));
    return screen.findByRole("status");
  };
  const green = "emerald";

  it("shows the result sentence under the row in a green tone", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked());
    renderPage();
    const note = await check();
    expect(note.textContent).toBe("Token accepted: default branch main, push allowed.");
    expect(note.className).toContain(green);
    expect(note.className).not.toContain("red");
    expect((await rowOf("yuno-playground")).nextElementSibling?.contains(note)).toBe(true);
    expect(api.made("POST", "/api/projects/p1/check")).toHaveLength(1);
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });

  it("stays green when the answer leaves push rights and the branch out", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked({ can_push: undefined, branch_exists: undefined }));
    renderPage();
    expect((await check()).className).toContain(green);
  });

  it.each([
    ["the token was refused", checked({ ok: false, message: "The token was refused (401).", can_push: undefined, branch_exists: undefined })],
    ["the token cannot push", checked({ message: "Token accepted, but it cannot push to main.", can_push: false })],
    ["the branch does not exist", checked({ message: "Token accepted, but branch main does not exist; the repository's default branch is develop.", branch_exists: false })],
  ])("shows a red tone when %s", async (_, result) => {
    api.on("POST", "/api/projects/p1/check", () => result);
    renderPage();
    const note = await check();
    expect(note.textContent).toBe(result.message);
    expect(note.className).toContain("red");
    expect(note.className).not.toContain(green);
  });

  it("checks one project at a time and keeps each result under its own row", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked());
    api.on("POST", "/api/projects/p2/check", () => checked({ ok: false, message: "Repository not found (404): check the URL and the token's scope.", can_push: undefined, branch_exists: undefined }));
    renderPage();
    await check();
    fireEvent.click(screen.getByRole("button", { name: "Check docs-site" }));
    await waitFor(() => expect(screen.getAllByRole("status")).toHaveLength(2));
    const second = (await rowOf("docs-site")).nextElementSibling as HTMLElement;
    expect(second.textContent).toContain("Repository not found (404)");
    expect(((await rowOf("yuno-playground")).nextElementSibling as HTMLElement).textContent).toContain("Token accepted");
  });

  it("reports a failed request in red", async () => {
    api.on("POST", "/api/projects/p1/check", () => apiError(500, "internal", "boom"));
    renderPage();
    const note = await check();
    expect(note.textContent).toBe("Could not check the project: boom");
    expect(note.className).toContain("red");
  });

  it("keeps the result while the form is opened and closed", async () => {
    api.on("POST", "/api/projects/p1/check", () => checked());
    renderPage();
    await check();
    fireEvent.click(screen.getByRole("link", { name: "Edit yuno-playground" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("status").textContent).toContain("Token accepted");
  });

  it("does not show a result that predates the last change to the project", async () => {
    projects = [project({ updated_at: "2026-10-16T11:00:00Z" })];
    api.on("POST", "/api/projects/p1/check", () => checked({ checked_at: "2026-10-16T10:00:00Z" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Check yuno-playground" }));
    await waitFor(() => expect(api.made("POST", "/api/projects/p1/check")).toHaveLength(1));
    await waitFor(() => expect((screen.getByRole("button", { name: "Check yuno-playground" }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByRole("status")).toBeNull();
  });
});

describe("deleting a project", () => {
  it("asks first, then removes it", async () => {
    api.on("DELETE", /^\/api\/projects\/([^/]+)$/, (r) => {
      projects = projects.filter((p) => p.id !== r.match[1]);
      return new Response(null, { status: 204 });
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete docs-site" }));
    let d = await dialog();
    expect(d.getByRole("heading", { name: "Delete docs-site?" })).not.toBeNull();
    expect(d.getByText("Runs that used this project keep their history.")).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(api.made("DELETE", /projects/)).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Delete docs-site" }));
    d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete project" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Delete docs-site" })).toBeNull());
    expect(api.made("DELETE", "/api/projects/p2")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "yuno-playground" })).not.toBeNull();
  });

  it("shows why the API refused", async () => {
    api.on("DELETE", /^\/api\/projects\/[^/]+$/, () => apiError(404, "not_found", "project not found"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete docs-site" }));
    const d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete project" }));
    expect((await d.findByRole("alert")).textContent).toContain("project not found");
  });
});

describe("the name link", () => {
  it("opens the project's page", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "docs-site" });
    expect(link.getAttribute("href")).toBe("/projects/p2");
    fireEvent.click(link);
    expect(await screen.findByText("project page")).not.toBeNull();
    expect(where()).toBe("/projects/p2");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("encodes an id that needs it", async () => {
    projects = [project({ id: "a/b", name: "odd" })];
    renderPage();
    expect((await screen.findByRole("link", { name: "odd" })).getAttribute("href")).toBe("/projects/a%2Fb");
  });
});

describe("the Spend column", () => {
  const spend = (day: number, month: number) => ({ day_usd: day, month_usd: month, day_until: "2026-10-18T00:00:00Z", month_until: "2026-11-01T00:00:00Z" });
  const lines = async (name: string) => {
    const cell = (await rowOf(name)).querySelectorAll("td")[5];
    return Array.from(cell.querySelectorAll("p")) as HTMLElement[];
  };

  it("shows today's and this month's spend over the caps, with two decimals", async () => {
    projects = [project({ max_cost_usd_per_day: 5, max_cost_usd_per_month: 100.5, spend: spend(1.2, 12) })];
    renderPage();
    expect((await lines("yuno-playground")).map((l) => l.textContent)).toEqual(["$1.20 / $5.00 today", "$12.00 / $100.50 this month"]);
  });

  it("shows the amounts alone, muted, when there is no cap", async () => {
    projects = [project({ spend: spend(0.5, 1234.5) })];
    renderPage();
    const [day, month] = await lines("yuno-playground");
    expect(day.textContent).toBe("$0.50 today");
    expect(month.textContent).toBe("$1,234.50 this month");
    expect(day.className).toContain("zinc-500");
    expect(month.className).toContain("zinc-500");
  });

  it("keeps the normal tone below 80 %, warns from 80 % and flags the cap as reached at 100 %", async () => {
    projects = [
      project({ id: "low", name: "low", max_cost_usd_per_day: 10, max_cost_usd_per_month: 10, spend: spend(7.99, 0) }),
      project({ id: "warn", name: "warn", max_cost_usd_per_day: 10, max_cost_usd_per_month: 10, spend: spend(8, 9.99) }),
      project({ id: "full", name: "full", max_cost_usd_per_day: 10, max_cost_usd_per_month: 10, spend: spend(10, 12) }),
    ];
    renderPage();
    const [lowDay, lowMonth] = await lines("low");
    for (const l of [lowDay, lowMonth]) {
      expect(l.className).not.toContain("amber");
      expect(l.className).not.toContain("red");
      expect(l.className).not.toContain("zinc-500");
    }
    const [warnDay, warnMonth] = await lines("warn");
    expect(warnDay.className).toContain("amber");
    expect(warnMonth.className).toContain("amber");
    const [fullDay, fullMonth] = await lines("full");
    expect(fullDay.className).toContain("red");
    expect(fullDay.getAttribute("title")).toContain("cap is reached");
    expect(fullMonth.className).toContain("red");
    expect(fullMonth.textContent).toBe("$12.00 / $10.00 this month");
  });

  it("tones each window by its own cap", async () => {
    projects = [project({ max_cost_usd_per_day: 1, spend: spend(1, 50) })];
    renderPage();
    const [day, month] = await lines("yuno-playground");
    expect(day.className).toContain("red");
    expect(month.className).toContain("zinc-500");
  });

  it("shows a dash for a project the API sent no spend for", async () => {
    renderPage();
    expect((await rowOf("yuno-playground")).querySelectorAll("td")[5].textContent).toBe("—");
  });
});

describe("the project form: instructions and budget", () => {
  const edit = async (name = "yuno-playground") => {
    fireEvent.click(await screen.findByRole("link", { name: `Edit ${name}` }));
    return dialog();
  };
  const save = (d: ReturnType<typeof within>) => fireEvent.click(d.getByRole("button", { name: "Save changes" }));
  const sent = () => api.made("PUT", "/api/projects/p1")[0].body as ProjectInput;

  beforeEach(() => {
    api.on("PUT", /^\/api\/projects\/([^/]+)$/, () => project({}));
  });

  it("explains the Instructions field and starts it empty on a new project", async () => {
    renderPage();
    const d = await openNew();
    const field = d.getByLabelText("Instructions") as HTMLTextAreaElement;
    expect(field.tagName).toBe("TEXTAREA");
    expect(field.value).toBe("");
    expect(d.getByText("Standing instructions for every run on this project: conventions, how to build and test, what to leave alone.")).not.toBeNull();
  });

  it("groups the two caps under one Budget label with the hint", async () => {
    renderPage();
    const d = await openNew();
    const group = d.getByRole("group", { name: "Budget" });
    expect(within(group).getByLabelText("Per day (USD)")).not.toBeNull();
    expect(within(group).getByLabelText("Per month (USD)")).not.toBeNull();
    expect(within(group).getByText("Leave empty for no cap. New runs are refused once the cap is reached; running runs finish.")).not.toBeNull();
    expect((within(group).getByLabelText("Per day (USD)") as HTMLInputElement).value).toBe("");
  });

  it("opens on the saved instructions and caps and sends them back", async () => {
    projects = [project({ instructions: "Run `make test` before you push.\nLeave vendor/ alone.", max_cost_usd_per_day: 5, max_cost_usd_per_month: 12.5 })];
    renderPage();
    const d = await edit();
    expect((d.getByLabelText("Instructions") as HTMLTextAreaElement).value).toBe("Run `make test` before you push.\nLeave vendor/ alone.");
    expect((d.getByLabelText("Per day (USD)") as HTMLInputElement).value).toBe("5");
    expect((d.getByLabelText("Per month (USD)") as HTMLInputElement).value).toBe("12.5");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(sent()).toMatchObject({ instructions: "Run `make test` before you push.\nLeave vendor/ alone.", max_cost_usd_per_day: 5, max_cost_usd_per_month: 12.5 });
  });

  it("sends what was typed, the instructions trimmed and the caps as numbers", async () => {
    renderPage();
    const d = await edit();
    fill(d, "Instructions", "  Use pnpm.  \n");
    fill(d, "Per day (USD)", " 2.50 ");
    fill(d, "Per month (USD)", "40");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(sent()).toMatchObject({ instructions: "Use pnpm.", max_cost_usd_per_day: 2.5, max_cost_usd_per_month: 40 });
  });

  it("sends null for a cap that is emptied, which clears it", async () => {
    projects = [project({ max_cost_usd_per_day: 5, max_cost_usd_per_month: 12.5 })];
    renderPage();
    const d = await edit();
    fill(d, "Per day (USD)", "");
    fill(d, "Per month (USD)", "   ");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    const body = sent() as Record<string, unknown>;
    expect(body.max_cost_usd_per_day).toBeNull();
    expect(body.max_cost_usd_per_month).toBeNull();
    expect("max_cost_usd_per_day" in body).toBe(true);
  });

  it.each([
    ["not a number", "abc", "Enter an amount in dollars"],
    ["a dollar sign", "$5", "Enter an amount in dollars"],
    ["zero", "0", "Must be greater than 0"],
    ["a negative number", "-3", "Must be greater than 0"],
  ])("refuses %s as a cap before sending", async (_, text, message) => {
    renderPage();
    const d = await edit();
    fill(d, "Per day (USD)", text);
    fill(d, "Per month (USD)", text);
    save(d);
    await waitFor(() => expect(d.getByLabelText("Per day (USD)").getAttribute("aria-invalid")).toBe("true"));
    expect(d.getByLabelText("Per day (USD)").parentElement?.textContent).toContain(message);
    expect(d.getByLabelText("Per month (USD)").getAttribute("aria-invalid")).toBe("true");
    expect(api.made("PUT", /./)).toHaveLength(0);
  });

  it("accepts instructions of exactly 8192 characters and refuses one more", async () => {
    renderPage();
    const d = await edit();
    fill(d, "Instructions", "x".repeat(8193));
    save(d);
    await waitFor(() => expect(d.getByLabelText("Instructions").getAttribute("aria-invalid")).toBe("true"));
    expect(d.getByLabelText("Instructions").parentElement?.textContent).toContain("Instructions must be at most 8192 characters");
    expect(api.made("PUT", /./)).toHaveLength(0);

    fill(d, "Instructions", "x".repeat(8192));
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(sent().instructions).toHaveLength(8192);
  });

  it("puts the API's 422 next to the field it names", async () => {
    api.on("PUT", /^\/api\/projects\/([^/]+)$/, () =>
      apiError(422, "validation_failed", "validation failed", [
        { path: "instructions", message: "at most 8192 bytes" },
        { path: "max_cost_usd_per_day", message: "at most 1000000" },
        { path: "max_cost_usd_per_month", message: "must be greater than 0" },
      ]),
    );
    renderPage();
    const d = await edit();
    fill(d, "Instructions", "é".repeat(5000));
    fill(d, "Per day (USD)", "2000000");
    fill(d, "Per month (USD)", "1");
    save(d);
    await waitFor(() => expect(d.getByLabelText("Instructions").parentElement?.textContent).toContain("at most 8192 bytes"));
    expect(d.getByLabelText("Per day (USD)").parentElement?.textContent).toContain("at most 1000000");
    expect(d.getByLabelText("Per month (USD)").parentElement?.textContent).toContain("must be greater than 0");
    expect(screen.getByRole("dialog")).not.toBeNull();
  });
});
