// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { Project } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import QuickRunDialog from "./QuickRunDialog";

let projects: Project[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  projects = [project({}), project({ id: "p2", name: "docs-site" })];
  api = installFakeApi();
  api.on("GET", "/api/projects", () => projects);
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
