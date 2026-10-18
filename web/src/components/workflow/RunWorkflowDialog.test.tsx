// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { MergeRequest, Project, WorkflowDefinition } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, project } from "@/test/fixtures";
import RunWorkflowDialog from "./RunWorkflowDialog";

const definition = (extra: Partial<WorkflowDefinition> = {}): WorkflowDefinition => ({
  nodes: [],
  edges: [],
  input_schema: { type: "object", properties: { task: { type: "string" } }, required: ["task"] },
  ...extra,
});

let projects: Project[];
let requests: MergeRequest[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  projects = [project({}), project({ id: "p2", name: "docs-site" })];
  requests = [
    mergeRequest({ id: "mr1", number: 12, title: "Add the health endpoint" }),
    mergeRequest({ id: "mr2", number: 9, title: "Fix the build" }),
    mergeRequest({ id: "mr3", project_id: "p2", number: 3, title: "Update the docs" }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/projects", () => projects);
  // Like the real list: narrowed to the project and the state when the query names them.
  api.on("GET", /^\/api\/merge-requests\?/, ({ url }) => {
    const query = new URL(url, "http://yuno.test").searchParams;
    return requests.filter((r) => r.project_id === query.get("project_id") && (!query.get("state") || r.state === query.get("state")));
  });
  api.on("POST", "/api/runs", () => ({ id: "run1" }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderDialog(def: WorkflowDefinition) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/workflows"]}>
        <Routes>
          <Route path="/workflows" element={<RunWorkflowDialog workflow={{ id: "w1", name: "Release train", definition: def }} onClose={() => undefined} />} />
          <Route path="/runs/:id" element={<p>a run</p>} />
          <Route path="/projects" element={<p>projects page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return within(screen.getByRole("dialog"));
}

const start = (d: ReturnType<typeof within>) => d.getByRole("button", { name: "Start run" }) as HTMLButtonElement;
const task = (d: ReturnType<typeof within>, value = "ship it") => fireEvent.change(d.getByLabelText(/^task/), { target: { value } });

describe("a workflow that works on a project", () => {
  const def = () => definition({ project: true });

  it("offers a required Project select of the live projects, with Choose a project first", async () => {
    const d = renderDialog(def());
    const select = (await d.findByLabelText(/^Project/)) as HTMLSelectElement;
    expect(select.parentElement?.textContent).toContain("*");
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Choose a project", "yuno-playground", "docs-site"]);
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "p1", "p2"]);
    expect(select.value).toBe("");
  });

  it("does not start until a project is chosen", async () => {
    const d = renderDialog(def());
    await d.findByLabelText(/^Project/);
    task(d);
    fireEvent.click(start(d));
    expect(await d.findByText("Choose a project", { selector: "p" })).not.toBeNull();
    expect(d.getByLabelText(/^Project/).getAttribute("aria-invalid")).toBe("true");
    expect(api.made("POST", "/api/runs")).toHaveLength(0);
  });

  it("starts the run with the chosen project's id next to the input", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p2" } });
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" }, project_id: "p2" });
  });

  it("still checks the input too", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.click(start(d));
    expect(await d.findByText("This field is required")).not.toBeNull();
    expect(api.made("POST", "/api/runs")).toHaveLength(0);
  });

  it("works without input fields too", async () => {
    const d = renderDialog(definition({ project: true, input_schema: { type: "object" } }));
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: {}, project_id: "p1" });
  });

  it("sends the user to the Projects page when none exists, and cannot start", async () => {
    projects = [];
    const d = renderDialog(def());
    const sentence = await d.findByText(/This workflow works on a project repository/);
    expect(sentence.textContent).toBe("This workflow works on a project repository. Create one on the Projects page first.");
    expect(within(sentence).getByRole("link", { name: "Projects page" }).getAttribute("href")).toBe("/projects");
    expect(d.queryByLabelText(/^Project/)).toBeNull();
    expect(start(d).disabled).toBe(true);
    fireEvent.click(within(sentence).getByRole("link", { name: "Projects page" }));
    expect(await screen.findByText("projects page")).not.toBeNull();
  });

  it("cannot start while the projects load, or when they cannot be loaded", async () => {
    api.on("GET", "/api/projects", () => apiError(500, "internal", "boom"));
    const d = renderDialog(def());
    expect((await d.findByRole("alert")).textContent).toContain("Could not load the projects: boom");
    expect(start(d).disabled).toBe(true);
  });

  it("shows the API's refusal of the project next to the select", async () => {
    api.on("POST", "/api/runs", () => apiError(422, "validation_failed", "validation failed", [{ path: "project_id", message: "no project with id p2" }]));
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p2" } });
    task(d);
    fireEvent.click(start(d));
    await waitFor(() => expect(d.getByLabelText(/^Project/).parentElement?.textContent).toContain("no project with id p2"));
    expect(d.getByLabelText(/^Project/).getAttribute("aria-invalid")).toBe("true");
    expect(d.queryByText("project_id: no project with id p2")).toBeNull();
  });

  it("forgets a chosen project that has been deleted since", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p2" } });
    projects = [projects[0]];
    api.on("POST", "/api/runs", () => apiError(422, "validation_failed", "validation failed", [{ path: "project_id", message: "no project with id p2" }]));
    task(d);
    fireEvent.click(start(d));
    await waitFor(() => expect((d.getByLabelText(/^Project/) as HTMLSelectElement).options).toHaveLength(2));
    expect((d.getByLabelText(/^Project/) as HTMLSelectElement).value).toBe("");
  });
});

describe("the merge request selector", () => {
  const def = () => definition({ project: true });
  const requestSelect = async (d: ReturnType<typeof within>) => (await d.findByLabelText("Merge request")) as HTMLSelectElement;

  it("is not there until a project is chosen, and asks for nothing before", async () => {
    const d = renderDialog(def());
    await d.findByLabelText(/^Project/);
    expect(d.queryByLabelText("Merge request")).toBeNull();
    expect(api.made("GET", /merge-requests/)).toHaveLength(0);
  });

  it("appears under the Project select once one is chosen, with None: open a new one selected and the project's open requests", async () => {
    const d = renderDialog(def());
    const project = (await d.findByLabelText(/^Project/)) as HTMLSelectElement;
    fireEvent.change(project, { target: { value: "p1" } });
    const select = await requestSelect(d);
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["None: open a new one", "!12 Add the health endpoint", "!9 Fix the build"]);
    expect(select.value).toBe("");
    expect(select.parentElement?.textContent).toContain("The run works on the request's branch and pushes onto it.");
    expect(project.compareDocumentPosition(select) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(api.made("GET", /merge-requests/).map((c) => c.url)).toEqual(["/api/merge-requests?project_id=p1&state=open&limit=200"]);
  });

  it("sends no merge_request_id while None stays chosen", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    await requestSelect(d);
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" }, project_id: "p1" });
  });

  it("sends the chosen request's id next to the project's", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr2" } });
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" }, project_id: "p1", merge_request_id: "mr2" });
  });

  it("goes back to None when another project is chosen, and lists that project's requests", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    fireEvent.change(d.getByLabelText(/^Project/), { target: { value: "p2" } });
    await waitFor(() => expect(Array.from((d.getByLabelText("Merge request") as HTMLSelectElement).options).map((o) => o.textContent)).toEqual(["None: open a new one", "!3 Update the docs"]));
    expect((d.getByLabelText("Merge request") as HTMLSelectElement).value).toBe("");
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" }, project_id: "p2" });
  });

  it("disappears again when the project is set back to Choose a project", async () => {
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    await requestSelect(d);
    fireEvent.change(d.getByLabelText(/^Project/), { target: { value: "" } });
    expect(d.queryByLabelText("Merge request")).toBeNull();
  });

  it("shows the API's refusal of the request next to the select, not as a loose line", async () => {
    api.on("POST", "/api/runs", () =>
      apiError(422, "validation_failed", "validation failed", [{ path: "merge_request_id", message: "merge request !12 is being updated by run 0192ab" }]),
    );
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    task(d);
    fireEvent.click(start(d));
    await waitFor(() => expect(d.getByLabelText("Merge request").parentElement?.textContent).toContain("merge request !12 is being updated by run 0192ab"));
    expect(d.getByLabelText("Merge request").getAttribute("aria-invalid")).toBe("true");
    expect(d.getByLabelText(/^Project/).getAttribute("aria-invalid")).toBeNull();
    expect(d.queryByText("merge_request_id: merge request !12 is being updated by run 0192ab")).toBeNull();
  });

  it("reads the requests again after a refusal, and forgets one that is no longer open", async () => {
    api.on("POST", "/api/runs", () => apiError(422, "validation_failed", "validation failed", [{ path: "merge_request_id", message: "merge request !12 is merged" }]));
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    requests = requests.map((r) => (r.id === "mr1" ? { ...r, state: "merged" as const } : r));
    task(d);
    fireEvent.click(start(d));
    await waitFor(() => expect(Array.from((d.getByLabelText("Merge request") as HTMLSelectElement).options).map((o) => o.textContent)).toEqual(["None: open a new one", "!9 Fix the build"]));
    expect((d.getByLabelText("Merge request") as HTMLSelectElement).value).toBe("");
    expect(d.getByLabelText("Merge request").parentElement?.textContent).toContain("merge request !12 is merged");
  });

  it("still starts the run when the requests cannot be loaded", async () => {
    api.on("GET", /^\/api\/merge-requests\?/, () => apiError(500, "internal", "boom"));
    const d = renderDialog(def());
    fireEvent.change(await d.findByLabelText(/^Project/), { target: { value: "p1" } });
    expect(await d.findByText(/Could not load the merge requests: boom/)).not.toBeNull();
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" }, project_id: "p1" });
  });
});

describe("a workflow that does not work on a project", () => {
  it("has no Project select, asks for no projects and sends no project_id", async () => {
    const d = renderDialog(definition());
    task(d);
    fireEvent.click(start(d));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(d.queryByLabelText(/^Project/)).toBeNull();
    expect(api.made("GET", "/api/projects")).toHaveLength(0);
    expect(d.queryByLabelText("Merge request")).toBeNull();
    expect(api.made("GET", /merge-requests/)).toHaveLength(0);
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship it" } });
  });

  it("shows no project sentence even when no project exists", async () => {
    projects = [];
    const d = renderDialog(definition({ project: false }));
    expect(d.queryByText(/works on a project repository/)).toBeNull();
    expect(start(d).disabled).toBe(false);
  });
});
