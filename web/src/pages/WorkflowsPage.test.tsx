// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useParams } from "react-router";
import type { Workflow, WorkflowTemplate } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import WorkflowsPage from "./WorkflowsPage";

const def = (nodes: number, extra: Record<string, unknown> = {}) => ({
  nodes: Array.from({ length: nodes }, (_, i) => ({ id: `n${i}`, kind: i === 0 ? "start" : "agent", ...extra })),
  edges: [],
  input_schema: { type: "object", properties: { task: { type: "string" } }, required: ["task"] },
});

let workflows: Workflow[];
let api: ReturnType<typeof installFakeApi>;

const wf = (over: Partial<Workflow>): Workflow =>
  ({ id: "w1", name: "Release train", description: "Ship it", definition: def(4), version: 2, created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T10:30:00Z", ...over }) as Workflow;

const templates: WorkflowTemplate[] = [
  {
    key: "dev_pipeline",
    name: "Dev pipeline",
    description: "Coder writes, Reviewer checks, a human approves the deploy.",
    definition: {
      nodes: [
        { id: "start", kind: "start" },
        { id: "coder", kind: "agent", agent_name: "Coder" },
        { id: "reviewer", kind: "agent", agent_name: "Reviewer" },
        { id: "end", kind: "end" },
      ],
      edges: [],
    },
  },
  { key: "research_pipeline", name: "Research pipeline", description: "Researcher, Critic, Writer.", definition: { nodes: [{ id: "start", kind: "start" }], edges: [] } },
];

function Opened() {
  const { id } = useParams();
  return <p>builder for {id}</p>;
}

beforeEach(() => {
  workflows = [
    wf({}),
    wf({ id: "w2", name: "Dev pipeline", version: 1, template_key: "dev_pipeline", definition: { nodes: [{ id: "c", kind: "agent", agent_name: "Coder" }], edges: [] } }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/workflows", () => workflows);
  api.on("GET", "/api/workflow-templates", () => templates);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/workflows"]}>
        <Routes>
          <Route path="/workflows" element={<WorkflowsPage />} />
          <Route path="/workflows/:id" element={<Opened />} />
          <Route path="/workflows/new" element={<p>new builder</p>} />
          <Route path="/runs/:id" element={<p>a run</p>} />
          <Route path="/agents" element={<p>agents page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("WorkflowsPage list", () => {
  it("lists name, version, node count and last update, with a badge on shipped templates", async () => {
    renderPage();
    const row = (await screen.findByRole("link", { name: "Release train" })).closest("tr")!;
    const cells = within(row).getAllByRole("cell");
    expect(cells[1].textContent).toBe("2");
    expect(cells[2].textContent).toBe("4");
    expect(cells[3].textContent).toBe(formatDateTime("2026-10-15T10:30:00Z"));
    expect(within(row).queryByText("Template")).toBeNull();

    const tpl = screen.getByRole("link", { name: "Dev pipeline" }).closest("tr")!;
    expect(within(tpl).getByText("Template")).not.toBeNull();
    expect(within(tpl).getByRole("button", { name: "Create a copy of Dev pipeline" })).not.toBeNull();
    expect(within(tpl).queryByRole("button", { name: /^Delete/ })).toBeNull();
    expect(within(tpl).queryByRole("button", { name: /^Run/ })).toBeNull();
  });

  it("marks a workflow made from a template once its agents are resolved", async () => {
    workflows = [wf({ template_key: "dev_pipeline", definition: { nodes: [{ id: "c", kind: "agent", agent_id: "a1" }], edges: [] } })];
    renderPage();
    const row = (await screen.findByRole("link", { name: "Release train" })).closest("tr")!;
    expect(within(row).getByText("From template")).not.toBeNull();
    expect(within(row).getByRole("button", { name: "Delete Release train" })).not.toBeNull();
  });

  it("marks a workflow that names the template it came from, whichever field the API uses", async () => {
    const resolved = { nodes: [{ id: "c", kind: "agent" as const, agent_id: "a1" }], edges: [] };
    workflows = [wf({ id: "w1", name: "Release train", from_template: "dev_pipeline", definition: resolved }), wf({ id: "w3", name: "Hand made", definition: resolved })];
    renderPage();
    const row = (await screen.findByRole("link", { name: "Release train" })).closest("tr")!;
    expect(within(row).getByText("From template").getAttribute("title")).toBe("Made from the dev_pipeline template");
    expect(within(screen.getByRole("link", { name: "Hand made" }).closest("tr")!).queryByText("From template")).toBeNull();
  });

  it("offers New workflow and says so when there are none", async () => {
    workflows = [];
    renderPage();
    expect(await screen.findByText(/No workflows yet/)).not.toBeNull();
    expect(screen.getByRole("link", { name: "New workflow" }).getAttribute("href")).toBe("/workflows/new");
  });

  it("makes an editable copy of a shipped template and opens it", async () => {
    api.on("POST", "/api/workflow-templates/dev_pipeline/instantiate", () => wf({ id: "w9", name: "Dev pipeline" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Create a copy of Dev pipeline" }));
    expect(await screen.findByText("builder for w9")).not.toBeNull();
  });
});

describe("From template", () => {
  it("lists the templates with their agents, instantiates one under the chosen name and opens it", async () => {
    api.on("POST", "/api/workflow-templates/dev_pipeline/instantiate", () => wf({ id: "w7", name: "My pipeline" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "From template" }));
    const drawer = within(await screen.findByRole("dialog", { name: "Start from a template" }));
    expect(await drawer.findByText("Research pipeline")).not.toBeNull();
    expect(drawer.getByText(/4 nodes · Agents: Coder, Reviewer/)).not.toBeNull();

    const card = within(drawer.getByText("Dev pipeline").closest("li")!);
    expect((card.getByLabelText("Workflow name") as HTMLInputElement).value).toBe("Dev pipeline");
    fireEvent.change(card.getByLabelText("Workflow name"), { target: { value: "My pipeline" } });
    fireEvent.click(card.getByRole("button", { name: "Use Dev pipeline" }));

    expect(await screen.findByText("builder for w7")).not.toBeNull();
    expect(api.made("POST", "/api/workflow-templates/dev_pipeline/instantiate")[0].body).toEqual({ name: "My pipeline" });
  });

  it("names the nodes whose agents do not exist and sends the operator to the agents page", async () => {
    api.on("POST", "/api/workflow-templates/dev_pipeline/instantiate", () =>
      apiError(422, "validation_failed", "template cannot be instantiated", [
        { path: "nodes[1].agent_name", message: "no agent named 'Coder'" },
        { path: "nodes[2].agent_name", message: "no agent named 'Reviewer'" },
      ]),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "From template" }));
    fireEvent.click(await screen.findByRole("button", { name: "Use Dev pipeline" }));
    const alert = within(await screen.findByRole("alert"));
    expect(alert.getByText("Node coder: no agent named 'Coder'")).not.toBeNull();
    expect(alert.getByText("Node reviewer: no agent named 'Reviewer'")).not.toBeNull();
    expect(alert.getByRole("link", { name: "Agents page" }).getAttribute("href")).toBe("/agents");
    expect(screen.queryByText(/builder for/)).toBeNull();
  });

  it("closes with Escape", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "From template" }));
    await screen.findByRole("dialog");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("deleting and running", () => {
  it("asks before deleting and refreshes the list", async () => {
    api.on("DELETE", "/api/workflows/w1", () => {
      workflows = workflows.filter((w) => w.id !== "w1");
      return new Response(null, { status: 204 });
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete Release train" }));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByText(/Runs that already used it keep their history/)).not.toBeNull();
    expect(api.made("DELETE", /./)).toHaveLength(0);
    fireEvent.click(dialog.getByRole("button", { name: "Delete workflow" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "Release train" })).toBeNull());
    expect(api.made("DELETE", "/api/workflows/w1")).toHaveLength(1);
  });

  it("keeps the workflow when the delete is cancelled", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete Release train" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.made("DELETE", /./)).toHaveLength(0);
  });

  it("runs a workflow from the list with the input its schema asks for", async () => {
    api.on("POST", "/api/runs", () => ({ id: "r1" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Run Release train" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText(/^task/), { target: { value: "ship v2" } });
    fireEvent.click(dialog.getByRole("button", { name: "Start run" }));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "w1", input: { task: "ship v2" } });
  });
});
