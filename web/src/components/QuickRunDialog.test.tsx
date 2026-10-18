// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { MergeRequest, Project } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, project } from "@/test/fixtures";
import QuickRunDialog from "./QuickRunDialog";

let projects: Project[];
let requests: MergeRequest[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  projects = [project({}), project({ id: "p2", name: "docs-site" })];
  requests = [
    mergeRequest({ id: "mr1", number: 12, title: "Add the health endpoint" }),
    mergeRequest({ id: "mr2", number: 9, title: "Fix the build", state: "merged" }),
    mergeRequest({ id: "mr3", project_id: "p2", provider: "github", number: 3, title: "Update the docs", url: "https://github.com/o/r/pull/3" }),
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

function renderDialog() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/agents"]}>
        <Routes>
          <Route path="/agents" element={<QuickRunDialog agent={{ id: "a1", name: "Coder" }} onClose={() => undefined} />} />
          <Route path="/runs/:id" element={<p>a run</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return within(screen.getByRole("dialog"));
}

const run = (d: ReturnType<typeof within>, task = "fix the build") => {
  fireEvent.change(d.getByLabelText("Task"), { target: { value: task } });
  fireEvent.click(d.getByRole("button", { name: "Start run" }));
};

describe("QuickRunDialog project select", () => {
  it("offers an optional Project select, None first, once projects exist", async () => {
    const d = renderDialog();
    const select = (await d.findByLabelText("Project")) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["None", "yuno-playground", "docs-site"]);
    expect(select.value).toBe("");
    expect(select.parentElement?.textContent).not.toContain("*");
  });

  it("starts without a project when None stays chosen", async () => {
    const d = renderDialog();
    await d.findByLabelText("Project");
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build" });
  });

  it("carries project_id when one is chosen", async () => {
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p2" } });
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build", project_id: "p2" });
  });

  it("has no select when there are no projects, and no longer asks for the platform status", async () => {
    projects = [];
    const d = renderDialog();
    await waitFor(() => expect(api.made("GET", "/api/projects")).toHaveLength(1));
    expect(d.queryByLabelText("Project")).toBeNull();
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build" });
    expect(api.made("GET", "/api/status")).toHaveLength(0);
  });

  it("still starts a run when the projects cannot be loaded", async () => {
    api.on("GET", "/api/projects", () => apiError(500, "internal", "boom"));
    const d = renderDialog();
    await waitFor(() => expect(api.made("GET", "/api/projects")).toHaveLength(1));
    expect(d.queryByLabelText("Project")).toBeNull();
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
  });

  it("shows the API's refusal of the project next to the select", async () => {
    api.on("POST", "/api/runs", () => apiError(422, "validation_failed", "validation failed", [{ path: "project_id", message: "no project with id p2" }]));
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p2" } });
    run(d);
    await waitFor(() => expect(d.getByLabelText("Project").parentElement?.textContent).toContain("no project with id p2"));
    expect(d.queryByText("validation failed")).toBeNull();
  });

  it("shows any other failure under the form", async () => {
    api.on("POST", "/api/runs", () => apiError(500, "internal", "the queue is full"));
    const d = renderDialog();
    await d.findByLabelText("Project");
    run(d);
    expect((await d.findByRole("alert")).textContent).toBe("the queue is full");
  });
});

describe("QuickRunDialog merge request select", () => {
  const requestSelect = async (d: ReturnType<typeof within>) => (await d.findByLabelText("Merge request")) as HTMLSelectElement;
  const labels = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.textContent);

  it("is not there while no project is chosen, and asks for nothing", async () => {
    const d = renderDialog();
    await d.findByLabelText("Project");
    expect(d.queryByLabelText("Merge request")).toBeNull();
    expect(api.made("GET", /merge-requests/)).toHaveLength(0);
  });

  it("appears under the Project select once one is chosen, with None: open a new one selected and the project's open requests only", async () => {
    const d = renderDialog();
    const project = (await d.findByLabelText("Project")) as HTMLSelectElement;
    fireEvent.change(project, { target: { value: "p1" } });
    const select = await requestSelect(d);
    expect(labels(select)).toEqual(["None: open a new one", "!12 Add the health endpoint"]);
    expect(select.value).toBe("");
    expect(select.parentElement?.textContent).toContain("The run works on the request's branch and pushes onto it.");
    expect(project.compareDocumentPosition(select) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(api.made("GET", /merge-requests/).map((c) => c.url)).toEqual(["/api/merge-requests?project_id=p1&state=open&limit=200"]);
  });

  it("writes a GitHub project's request with #", async () => {
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p2" } });
    expect(labels(await requestSelect(d))).toEqual(["None: open a new one", "#3 Update the docs"]);
  });

  it("sends no merge_request_id while None stays chosen", async () => {
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p1" } });
    await requestSelect(d);
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build", project_id: "p1" });
  });

  it("sends the chosen request's id next to the project's", async () => {
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build", project_id: "p1", merge_request_id: "mr1" });
  });

  it("goes back to None, and sends no request, when another project or None is chosen", async () => {
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    fireEvent.change(d.getByLabelText("Project"), { target: { value: "p2" } });
    await waitFor(() => expect(labels(d.getByLabelText("Merge request") as HTMLSelectElement)).toEqual(["None: open a new one", "#3 Update the docs"]));
    expect((d.getByLabelText("Merge request") as HTMLSelectElement).value).toBe("");

    fireEvent.change(d.getByLabelText("Merge request"), { target: { value: "mr3" } });
    fireEvent.change(d.getByLabelText("Project"), { target: { value: "" } });
    expect(d.queryByLabelText("Merge request")).toBeNull();
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build" });
  });

  it("shows the API's refusal of the request next to the select, not as a loose line", async () => {
    api.on("POST", "/api/runs", () =>
      apiError(422, "validation_failed", "validation failed", [{ path: "merge_request_id", message: "merge request !12 belongs to project docs-site" }]),
    );
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p1" } });
    fireEvent.change(await requestSelect(d), { target: { value: "mr1" } });
    run(d);
    await waitFor(() => expect(d.getByLabelText("Merge request").parentElement?.textContent).toContain("merge request !12 belongs to project docs-site"));
    expect(d.getByLabelText("Merge request").getAttribute("aria-invalid")).toBe("true");
    expect(d.queryByText("merge_request_id: merge request !12 belongs to project docs-site")).toBeNull();
    expect(d.getByLabelText("Project").getAttribute("aria-invalid")).toBeNull();
  });

  it("still starts a run when the requests cannot be loaded", async () => {
    api.on("GET", /^\/api\/merge-requests\?/, () => apiError(500, "internal", "boom"));
    const d = renderDialog();
    fireEvent.change(await d.findByLabelText("Project"), { target: { value: "p1" } });
    expect(await d.findByText(/Could not load the merge requests: boom/)).not.toBeNull();
    run(d);
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the build", project_id: "p1" });
  });
});
