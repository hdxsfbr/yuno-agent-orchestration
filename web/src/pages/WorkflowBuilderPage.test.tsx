// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation, useParams } from "react-router";
import type { Agent, Workflow, WorkflowDefinition } from "@/lib/api";
import { apiError, installFakeApi, json, mockReactFlowEnvironment } from "@/test/fakeApi";
import WorkflowBuilderPage from "./WorkflowBuilderPage";

const agents = [
  { id: "a1", name: "Coder", model: "claude-opus-5-5" },
  { id: "a2", name: "Reviewer", model: "claude-opus-5-5" },
  { id: "a3", name: "Deployer", model: "claude-opus-5-5" },
] as Agent[];

const definition = (): WorkflowDefinition =>
  ({
    input_schema: {
      type: "object",
      properties: { task: { type: "string" }, retries: { type: "integer" }, dry_run: { type: "boolean" }, options: { type: "object" } },
      required: ["task"],
    },
    state_schema: { type: "object", properties: { reviewer: { type: "object" } } },
    nodes: [
      { id: "start", kind: "start" },
      { id: "coder", kind: "agent", agent_id: "a1", task: "Build {{.input.task}}", output_schema: { type: "object" }, input_from: ["start", "reviewer"], note: "keep me" },
      { id: "reviewer", kind: "agent", agent_id: "a2", task: "Review it", input_from: ["coder"] },
      { id: "gate", kind: "approval", message: "Deploy?", on_reject: "coder" },
      { id: "end", kind: "end" },
    ],
    edges: [
      { from: "start", to: "coder" },
      { from: "coder", to: "reviewer" },
      { from: "reviewer", to: "coder", when: { path: "/reviewer/verdict", op: "eq", value: "changes_requested" } },
      { from: "reviewer", to: "gate", when: { path: "/reviewer/verdict", op: "eq", value: "approved" } },
      { from: "gate", to: "end" },
    ],
    limits: { max_steps: 30, max_cost_usd: 1.5 },
    custom_member: { keep: true },
  }) as WorkflowDefinition;

const workflow = (over: Partial<Workflow> = {}): Workflow => ({
  id: "wf1",
  name: "Dev pipeline",
  description: "Build and ship",
  definition: definition(),
  version: 3,
  created_at: "2026-10-15T10:00:00Z",
  updated_at: "2026-10-15T11:00:00Z",
  ...over,
});

let api: ReturnType<typeof installFakeApi>;

function RunMarker() {
  const { id } = useParams();
  return <p>run page {id}</p>;
}
function Where() {
  return <p>at {useLocation().pathname}</p>;
}

beforeEach(() => {
  mockReactFlowEnvironment();
  api = installFakeApi();
  api.on("GET", "/api/agents", () => agents);
  api.on("GET", "/api/workflows/wf1", () => workflow());
  api.on("PUT", "/api/workflows/wf1", (r) => workflow({ ...(r.body as object), version: 4 }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderBuilder(path = "/workflows/wf1") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/workflows/new" element={<WorkflowBuilderPage />} />
          <Route path="/workflows/:id" element={<WorkflowBuilderPage />} />
          <Route path="/runs/:id" element={<RunMarker />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const outline = async () => within(await screen.findByRole("region", { name: "Outline" }));
const save = () => fireEvent.click(screen.getByRole("button", { name: "Save" }));
const saved = () => api.made("PUT", "/api/workflows/wf1")[0].body as { name: string; description: string; definition: WorkflowDefinition & Record<string, unknown> };

describe("opening a workflow", () => {
  it("lists its nodes and edges, with the order of edges that share a source", async () => {
    renderBuilder();
    const o = await outline();
    for (const id of ["start", "coder", "reviewer", "gate", "end"]) expect(o.getByRole("button", { name: `Node ${id}` })).not.toBeNull();
    expect(o.getByRole("button", { name: "Edge reviewer to coder, order 1" })).not.toBeNull();
    expect(o.getByRole("button", { name: "Edge reviewer to gate, order 2" })).not.toBeNull();
    expect(o.getByRole("button", { name: "Edge start to coder" })).not.toBeNull();
    expect((screen.getByLabelText(/^Name/) as HTMLInputElement).value).toBe("Dev pipeline");
    expect(screen.getByText("Version 3")).not.toBeNull();
  });

  it("draws every node on the canvas", async () => {
    renderBuilder();
    await outline();
    const canvas = screen.getByRole("group", { name: "Workflow canvas" });
    await waitFor(() => expect(canvas.querySelectorAll(".react-flow__node")).toHaveLength(5));
  });

  it("shows the seven limits and both schemas", async () => {
    renderBuilder();
    await outline();
    expect((screen.getByLabelText("Max steps") as HTMLInputElement).value).toBe("30");
    expect((screen.getByLabelText("Max cost (USD)") as HTMLInputElement).value).toBe("1.5");
    for (const l of ["Max duration (seconds)", "Max node iterations", "Max loop visits", "Max tool calls", "Approval timeout (seconds)"]) {
      expect((screen.getByLabelText(l) as HTMLInputElement).value).toBe("");
    }
    expect((screen.getByLabelText("Input schema") as HTMLTextAreaElement).value).toContain('"retries"');
    expect((screen.getByLabelText("State schema") as HTMLTextAreaElement).value).toContain('"reviewer"');
  });

  it("offers a copy instead of saving when the workflow is a shipped template", async () => {
    const tpl = workflow({ template_key: "dev_pipeline" });
    tpl.definition.nodes[1] = { id: "coder", kind: "agent", agent_name: "Coder", task: "x" };
    api.on("GET", "/api/workflows/wf1", () => tpl);
    api.on("POST", "/api/workflow-templates/dev_pipeline/instantiate", () => workflow({ id: "wf2", name: "Dev pipeline copy" }));
    api.on("GET", "/api/workflows/wf2", () => workflow({ id: "wf2", name: "Dev pipeline copy" }));
    renderBuilder();
    const makeCopy = await screen.findByRole("button", { name: "Create editable copy" });
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "edited" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Run" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(makeCopy);
    await waitFor(() => expect((screen.getByLabelText(/^Name/) as HTMLInputElement).value).toBe("Dev pipeline copy"));
    expect(screen.queryByRole("button", { name: "Create editable copy" })).toBeNull();
    expect(api.made("POST", "/api/workflow-templates/dev_pipeline/instantiate")).toHaveLength(1);
  });
});

describe("editing and saving", () => {
  it("saves an edited condition with every other part of the definition unchanged", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 1" }));
    const panel = within(screen.getByRole("region", { name: "Edge settings" }));
    expect(panel.getByTestId("edge-order").textContent).toBe("1 of 2");
    fireEvent.change(panel.getByLabelText("Value"), { target: { value: "needs_work" } });
    fireEvent.change(panel.getByLabelText("Check"), { target: { value: "neq" } });
    save();

    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const body = saved();
    const original = definition();
    expect(body.definition.edges[2]).toEqual({ from: "reviewer", to: "coder", when: { path: "/reviewer/verdict", op: "neq", value: "needs_work" } });
    expect(body.definition.edges.filter((_, i) => i !== 2)).toEqual(original.edges.filter((_, i) => i !== 2));
    expect(body.definition.nodes).toEqual(original.nodes);
    expect(body.definition.limits).toEqual(original.limits);
    expect(body.definition.input_schema).toEqual(original.input_schema);
    expect(body.definition.state_schema).toEqual(original.state_schema);
    expect(body.definition.custom_member).toEqual({ keep: true });
    expect(body.name).toBe("Dev pipeline");
    expect(body.description).toBe("Build and ship");
    expect(await screen.findByText("Changes saved.")).not.toBeNull();
    expect(screen.getByText("Version 4")).not.toBeNull();
  });

  it("remembers where every node is drawn under ui.positions", async () => {
    renderBuilder();
    await outline();
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "Build, review, ship" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const positions = (saved().definition.ui as { positions: Record<string, { x: number; y: number }> }).positions;
    expect(Object.keys(positions).sort()).toEqual(["coder", "end", "gate", "reviewer", "start"]);
    expect(positions.start.x).toBeLessThan(positions.coder.x);
    expect(positions.coder.x).toBeLessThan(positions.reviewer.x);
    expect(positions.reviewer.x).toBeLessThan(positions.gate.x);
    expect(positions.gate.x).toBeLessThan(positions.end.x);
  });

  it("keeps the save button off until something changes", async () => {
    renderBuilder();
    await outline();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "x" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText("Unsaved changes")).not.toBeNull();
  });

  it("edits an agent node: agent, task, output schema, inputs and failure target", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node coder" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    fireEvent.change(panel.getByLabelText(/^Agent/), { target: { value: "a3" } });
    fireEvent.change(panel.getByLabelText(/^Task/), { target: { value: "New task for {{.state.reviewer.feedback}}" } });
    fireEvent.change(panel.getByLabelText("Output schema"), { target: { value: '{"type":"object","required":["summary"]}' } });
    fireEvent.click(panel.getByRole("checkbox", { name: /^reviewer/ }));
    fireEvent.change(panel.getByLabelText("If this node fails, go to"), { target: { value: "end" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.nodes[1]).toEqual({
      id: "coder",
      kind: "agent",
      agent_id: "a3",
      task: "New task for {{.state.reviewer.feedback}}",
      output_schema: { type: "object", required: ["summary"] },
      input_from: ["start"],
      on_failure: "end",
      note: "keep me",
    });
  });

  it("explains the template placeholders next to the task", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node coder" }));
    const hint = screen.getByText(/for a value from the run input/);
    expect(hint.textContent).toContain("{{.input.field}}");
    expect(hint.textContent).toContain("{{.state.node_id.field}}");
  });

  it("edits an approval node: message and reject target", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node gate" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    fireEvent.change(panel.getByLabelText("Message"), { target: { value: "Ship {{.state.coder.summary}}?" } });
    fireEvent.change(panel.getByLabelText("If rejected, go to"), { target: { value: "" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const gate = saved().definition.nodes[3];
    expect(gate.message).toBe("Ship {{.state.coder.summary}}?");
    expect("on_reject" in gate).toBe(false);
  });

  it("renames a node everywhere it is mentioned", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node coder" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    fireEvent.change(panel.getByLabelText("Node id"), { target: { value: "Bad Id" } });
    expect(panel.getByText(/lowercase letters, digits or underscores/)).not.toBeNull();
    fireEvent.change(panel.getByLabelText("Node id"), { target: { value: "reviewer" } });
    expect(panel.getByText("Another node is already called reviewer")).not.toBeNull();
    fireEvent.change(panel.getByLabelText("Node id"), { target: { value: "writer" } });
    expect(panel.queryByText(/already called/)).toBeNull();
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const def = saved().definition;
    expect(def.nodes.map((n) => n.id)).toEqual(["start", "writer", "reviewer", "gate", "end"]);
    expect(def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>writer", "writer>reviewer", "reviewer>writer", "reviewer>gate", "gate>end"]);
    expect(def.nodes[3].on_reject).toBe("writer");
    expect(def.nodes[2].input_from).toEqual(["writer"]);
  });

  it("renaming a node rewrites the conditions and templates that read its output, and says so", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node gate" }));
    fireEvent.change(within(screen.getByRole("region", { name: "Node settings" })).getByLabelText("Message"), {
      target: { value: "Deploy? {{.state.reviewer.feedback}}" },
    });
    fireEvent.click((await outline()).getByRole("button", { name: "Node reviewer" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    fireEvent.change(panel.getByLabelText("Node id"), { target: { value: "critic" } });
    expect(panel.getByRole("status").textContent).toBe("Updated 3 references in conditions and templates to use critic.");
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const def = saved().definition;
    expect(def.edges[2].when).toEqual({ path: "/critic/verdict", op: "eq", value: "changes_requested" });
    expect(def.edges[3].when).toEqual({ path: "/critic/verdict", op: "eq", value: "approved" });
    expect(def.nodes[3].message).toBe("Deploy? {{.state.critic.feedback}}");
  });

  it("changes a limit and clears it again", async () => {
    renderBuilder();
    await outline();
    fireEvent.change(screen.getByLabelText("Max loop visits"), { target: { value: "5" } });
    fireEvent.change(screen.getByLabelText("Max steps"), { target: { value: "" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.limits).toEqual({ max_cost_usd: 1.5, max_loop_visits: 5 });
  });

  it("says a run of the workflow names a project", async () => {
    renderBuilder();
    await outline();
    expect(
      screen.getByText(
        "Each run of this workflow names a project, chosen when the run starts. The project's repository is cloned into the first agent's run directory, later agents receive the checkout and the Deployer can open a merge request.",
      ),
    ).not.toBeNull();
  });

  it("opts the workflow into the project repository and out again, leaving the key out when off", async () => {
    renderBuilder();
    await outline();
    const box = screen.getByLabelText(/Runs work on the project repository/) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.project).toBe(true);
    fireEvent.click(box);
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(2));
    expect("project" in (api.made("PUT", "/api/workflows/wf1")[1].body as { definition: Record<string, unknown> }).definition).toBe(false);
  });

  it("holds back save while a limit is not a number", async () => {
    renderBuilder();
    await outline();
    fireEvent.change(screen.getByLabelText("Max steps"), { target: { value: "lots" } });
    expect(screen.getByText("Max steps must be a whole number")).not.toBeNull();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Max steps"), { target: { value: "12" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("deletes a node after confirmation along with its edges", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node gate" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete node" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete node" }));
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    const def = saved().definition;
    expect(def.nodes.map((n) => n.id)).toEqual(["start", "coder", "reviewer", "end"]);
    expect(def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>coder", "coder>reviewer", "reviewer>coder"]);
    expect("on_reject" in def.nodes[1]).toBe(false);
  });
});

describe("edge order and conditions", () => {
  it("reorders the edges of a node and saves them in that order", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node reviewer" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    expect((panel.getByRole("button", { name: "Move edge to coder earlier" }) as HTMLButtonElement).disabled).toBe(true);
    expect((panel.getByRole("button", { name: "Move edge to gate later" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(panel.getByRole("button", { name: "Move edge to gate earlier" }));

    const o = await outline();
    expect(o.getByRole("button", { name: "Edge reviewer to gate, order 1" })).not.toBeNull();
    expect(o.getByRole("button", { name: "Edge reviewer to coder, order 2" })).not.toBeNull();
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>coder", "coder>reviewer", "reviewer>gate", "reviewer>coder", "gate>end"]);
    expect(saved().definition.edges[3].when).toEqual({ path: "/reviewer/verdict", op: "eq", value: "changes_requested" });
  });

  it("reorders from the edge panel and keeps the edge selected", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 1" }));
    const panel = within(screen.getByRole("region", { name: "Edge settings" }));
    const later = panel.getByRole("button", { name: "Move later" });
    fireEvent.click(later);
    // the same panel stays open on the same edge, now second
    expect(screen.getByRole("region", { name: "Edge settings" })).toBe(panel.getByTestId("edge-order").closest("section"));
    expect(panel.getByRole("button", { name: "Move later" })).toBe(later);
    expect(panel.getByTestId("edge-order").textContent).toBe("2 of 2");
    expect((later as HTMLButtonElement).disabled).toBe(true);
    expect(panel.getByRole("heading", { name: /reviewer → coder/ })).not.toBeNull();
    expect((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 2" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("keeps the number and text types of a typed value, and edits a list for 'is one of'", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 1" }));
    const panel = within(screen.getByRole("region", { name: "Edge settings" }));
    fireEvent.change(panel.getByLabelText("Check"), { target: { value: "gt" } });
    fireEvent.change(panel.getByLabelText("Path"), { target: { value: "/reviewer/score" } });
    // typing a decimal passes through "3.0", which reads as the number 3; the box must not snap back
    for (const typed of ["3", "3.", "3.0"]) fireEvent.change(panel.getByLabelText("Value"), { target: { value: typed } });
    expect((panel.getByLabelText("Value") as HTMLInputElement).value).toBe("3.0");
    fireEvent.change(panel.getByLabelText("Value"), { target: { value: "3.05" } });
    expect((panel.getByLabelText("Value") as HTMLInputElement).value).toBe("3.05");
    fireEvent.change(panel.getByLabelText("Check"), { target: { value: "in" } });
    fireEvent.change(panel.getByLabelText("Values"), { target: { value: "approved, 3, \"4\"" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.edges[2].when).toEqual({ path: "/reviewer/score", op: "in", value: ["approved", 3, "4"] });
  });

  it("drops the condition when the edge is made unconditional", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 1" }));
    fireEvent.change(screen.getByLabelText("When to follow"), { target: { value: "always" } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect("when" in saved().definition.edges[2]).toBe(false);
  });

  it("falls back to JSON for all/any and says why", async () => {
    const wf = workflow();
    wf.definition.edges[3] = {
      from: "reviewer",
      to: "gate",
      when: { all: [{ path: "/reviewer/verdict", op: "eq", value: "approved" }, { any: [{ path: "/coder/tests_passed", op: "eq", value: true }] }] },
    };
    api.on("GET", "/api/workflows/wf1", () => wf);
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to gate, order 2" }));
    const panel = within(screen.getByRole("region", { name: "Edge settings" }));
    expect(panel.getByRole("note").textContent).toMatch(/combines several checks/);
    const box = panel.getByLabelText("Condition JSON") as HTMLTextAreaElement;
    expect(JSON.parse(box.value)).toEqual(wf.definition.edges[3].when);

    fireEvent.change(box, { target: { value: '{"any": [{"path": "/reviewer/verdict", "op": "exists"}]}' } });
    save();
    await waitFor(() => expect(api.made("PUT", "/api/workflows/wf1")).toHaveLength(1));
    expect(saved().definition.edges[3].when).toEqual({ any: [{ path: "/reviewer/verdict", op: "exists" }] });
  });

  it("shows JSON mistakes inline and holds back save until they are fixed", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Node coder" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    const box = panel.getByLabelText("Output schema");
    fireEvent.change(box, { target: { value: '{"type": ' } });
    expect(panel.getByText(/^Not valid JSON:/)).not.toBeNull();
    expect(screen.getByText(/Fix this before saving or running: Output schema is not valid JSON/)).not.toBeNull();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Validate" }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(box, { target: { value: '{"type": "object", "required": ["a"]}' } });
    expect(panel.queryByText(/^Not valid JSON:/)).toBeNull();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("flags a path that does not start with a slash", async () => {
    renderBuilder();
    fireEvent.click((await outline()).getByRole("button", { name: "Edge reviewer to coder, order 1" }));
    fireEvent.change(screen.getByLabelText("Path"), { target: { value: "reviewer/verdict" } });
    expect(screen.getByText(/Start the path with \//)).not.toBeNull();
  });
});

describe("building a new workflow", () => {
  it("starts as start to end, adds an agent, connects it and creates the workflow", async () => {
    api.on("POST", "/api/workflows", (r) => workflow({ ...(r.body as object), id: "wf9", version: 1 }));
    api.on("GET", "/api/workflows/wf9", () => workflow({ id: "wf9", name: "Fresh" }));
    renderBuilder("/workflows/new");
    const o = await outline();
    expect(o.getByRole("button", { name: "Node start" })).not.toBeNull();
    expect(o.getByRole("button", { name: "Node end" })).not.toBeNull();
    expect(o.getByRole("button", { name: "Edge start to end" })).not.toBeNull();

    // name is required
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("Name is required")).not.toBeNull();
    expect(api.made("POST", "/api/workflows")).toHaveLength(0);
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "Fresh" } });

    fireEvent.click(screen.getByRole("button", { name: "Add agent" }));
    const panel = within(screen.getByRole("region", { name: "Node settings" }));
    expect((panel.getByLabelText("Node id") as HTMLInputElement).value).toBe("agent");
    fireEvent.change(panel.getByLabelText(/^Agent/), { target: { value: "a2" } });
    fireEvent.change(panel.getByLabelText(/^Task/), { target: { value: "Do {{.input.task}}" } });
    fireEvent.change(panel.getByLabelText("Add edge to"), { target: { value: "end" } });
    // the new edge opens in the edge panel; connect start to the agent from the start node
    fireEvent.click((await outline()).getByRole("button", { name: "Node start" }));
    fireEvent.change(within(screen.getByRole("region", { name: "Node settings" })).getByLabelText("Add edge to"), { target: { value: "agent" } });

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.made("POST", "/api/workflows")).toHaveLength(1));
    const body = api.made("POST", "/api/workflows")[0].body as { name: string; definition: WorkflowDefinition };
    expect(body.name).toBe("Fresh");
    expect(body.definition.nodes).toEqual([
      { id: "start", kind: "start" },
      { id: "end", kind: "end" },
      { id: "agent", kind: "agent", agent_id: "a2", task: "Do {{.input.task}}" },
    ]);
    expect(body.definition.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>end", "agent>end", "start>agent"]);
    expect(body.definition.ui?.positions?.agent).toBeDefined();
    expect(await screen.findByText("Workflow created.")).not.toBeNull();
  });
});

describe("validation", () => {
  const details = [
    { path: "nodes[1].task", message: "task cannot be empty" },
    { path: "edges[2].to", message: "unknown node 'coderr'" },
    { path: "nodes", message: "exactly one start node required, found 0" },
  ];

  it("shows each message next to the node or edge it names, and in a list", async () => {
    api.on("POST", "/api/workflows/validate", () => apiError(422, "validation_failed", "definition is invalid", details));
    renderBuilder();
    const o = await outline();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    const problems = within(await screen.findByRole("region", { name: "Problems" }));
    expect(problems.getByText("3 problems to fix")).not.toBeNull();
    expect(problems.getByText("nodes[1].task")).not.toBeNull();
    expect(problems.getByText("edges[2].to")).not.toBeNull();
    expect(problems.getByText("exactly one start node required, found 0")).not.toBeNull();
    expect(api.made("POST", "/api/workflows/validate")[0].body).toMatchObject({ definition: { nodes: expect.any(Array) } });

    // the outline marks the node and the edge, and the canvas marks the node
    expect(within(o.getByRole("button", { name: "Node coder" })).getByText("1")).not.toBeNull();
    expect(within(o.getByRole("button", { name: "Edge reviewer to coder, order 1" })).getByText("1")).not.toBeNull();
    expect(within(o.getByRole("button", { name: "Node reviewer" })).queryByText("1")).toBeNull();
    await waitFor(() => expect(screen.getAllByTitle("Validation problems on this node")).toHaveLength(1));

    // opening the node shows the message under the task field
    fireEvent.click(problems.getByText("nodes[1].task"));
    const node = within(screen.getByRole("region", { name: "Node settings" }));
    expect(node.getByText("task cannot be empty")).not.toBeNull();
    expect(node.getByLabelText(/^Task/).getAttribute("aria-invalid")).toBe("true");

    // and the edge's message sits under its To field
    fireEvent.click(within(screen.getByRole("region", { name: "Problems" })).getByText("edges[2].to"));
    const edge = within(screen.getByRole("region", { name: "Edge settings" }));
    expect(edge.getByText("unknown node 'coderr'")).not.toBeNull();
    expect(edge.getByLabelText("To").getAttribute("aria-invalid")).toBe("true");
  });

  it("takes a node's messages away once that node is edited", async () => {
    api.on("POST", "/api/workflows/validate", () => apiError(422, "validation_failed", "definition is invalid", details));
    renderBuilder();
    fireEvent.click((await outline() , screen.getByRole("button", { name: "Validate" })) as unknown as HTMLElement);
    const problems = await screen.findByRole("region", { name: "Problems" });
    fireEvent.click(within(problems).getByText("nodes[1].task"));
    fireEvent.change(within(screen.getByRole("region", { name: "Node settings" })).getByLabelText(/^Task/), { target: { value: "Now it has a task" } });
    const left = within(screen.getByRole("region", { name: "Problems" }));
    expect(left.getByText("2 problems to fix")).not.toBeNull();
    expect(left.queryByText("nodes[1].task")).toBeNull();
    expect(screen.queryByText("task cannot be empty")).toBeNull();
  });

  it("reports a valid definition", async () => {
    api.on("POST", "/api/workflows/validate", () => ({ valid: true }));
    renderBuilder();
    await outline();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText("The definition is valid.")).not.toBeNull();
    expect(screen.queryByRole("region", { name: "Problems" })).toBeNull();
  });

  it("shows the same messages when save is refused", async () => {
    api.on("PUT", "/api/workflows/wf1", () => apiError(422, "validation_failed", "definition is invalid", [{ path: "edges[3].to", message: "unknown node 'gat'" }, { path: "name", message: "name already taken" }]));
    renderBuilder();
    await outline();
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "x" } });
    save();
    const problems = within(await screen.findByRole("region", { name: "Problems" }));
    expect(problems.getByText("edges[3].to")).not.toBeNull();
    expect(screen.getByLabelText(/^Name/).getAttribute("aria-invalid")).toBe("true");
    expect(screen.getAllByText("name already taken").length).toBeGreaterThan(0);
  });
});

describe("running a workflow", () => {
  it("builds the input form from the input schema and starts the run", async () => {
    api.on("POST", "/api/runs", () => ({ id: "run42" }));
    renderBuilder();
    await outline();
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog"));
    expect((dialog.getByLabelText(/^task/) as HTMLInputElement).type).toBe("text");
    fireEvent.change(dialog.getByLabelText(/^task/), { target: { value: "Write fizzbuzz" } });
    fireEvent.change(dialog.getByLabelText("retries"), { target: { value: "2" } });
    fireEvent.click(dialog.getByRole("checkbox", { name: "dry_run" }));
    fireEvent.change(dialog.getByLabelText("options"), { target: { value: '{"fast": true}' } });
    fireEvent.click(dialog.getByRole("button", { name: "Start run" }));

    expect(await screen.findByText("run page run42")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({
      workflow_id: "wf1",
      input: { task: "Write fizzbuzz", retries: 2, dry_run: true, options: { fast: true } },
    });
  });

  it("refuses a missing required field and bad JSON before calling the API", async () => {
    renderBuilder();
    await outline();
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("options"), { target: { value: "{" } });
    fireEvent.click(dialog.getByRole("button", { name: "Start run" }));
    expect(dialog.getByText("This field is required")).not.toBeNull();
    expect(dialog.getByText(/^Not valid JSON/)).not.toBeNull();
    expect(api.made("POST", "/api/runs")).toHaveLength(0);
  });

  it("shows the API's input errors under the fields they name", async () => {
    api.on("POST", "/api/runs", () => apiError(422, "validation_failed", "input does not match the schema", [{ path: "input.retries", message: "must be at most 5" }]));
    renderBuilder();
    await outline();
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText(/^task/), { target: { value: "x" } });
    fireEvent.change(dialog.getByLabelText("retries"), { target: { value: "9" } });
    fireEvent.click(dialog.getByRole("button", { name: "Start run" }));
    expect(await dialog.findByText("must be at most 5")).not.toBeNull();
    expect(dialog.getByLabelText("retries").getAttribute("aria-invalid")).toBe("true");
  });

  it("takes the whole input as JSON when the schema names no properties", async () => {
    const wf = workflow();
    wf.definition.input_schema = { type: "object" };
    api.on("GET", "/api/workflows/wf1", () => wf);
    api.on("POST", "/api/runs", () => ({ id: "run7" }));
    renderBuilder();
    await outline();
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("Input"), { target: { value: '{"anything": 1}' } });
    fireEvent.click(dialog.getByRole("button", { name: "Start run" }));
    expect(await screen.findByText("run page run7")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ workflow_id: "wf1", input: { anything: 1 } });
  });

  it("cannot run until the changes are saved", async () => {
    renderBuilder();
    await outline();
    const run = () => screen.getByRole("button", { name: "Run" }) as HTMLButtonElement;
    expect(run().disabled).toBe(false);
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "changed" } });
    expect(run().disabled).toBe(true);
    save();
    await waitFor(() => expect(run().disabled).toBe(false));
  });
});

describe("loading", () => {
  it("shows an error with a way back when the workflow cannot be loaded", async () => {
    api.on("GET", "/api/workflows/wf1", () => apiError(404, "not_found", "workflow not found"));
    renderBuilder();
    expect(await screen.findByText(/workflow not found/)).not.toBeNull();
    expect(screen.getByRole("link", { name: "Back to workflows" })).not.toBeNull();
  });

  it("falls back to a plain message when the save fails for another reason", async () => {
    api.on("PUT", "/api/workflows/wf1", () => json({ error: { code: "internal", message: "database is down" } }, 500));
    renderBuilder();
    await outline();
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "x" } });
    save();
    expect(await screen.findByText("database is down")).not.toBeNull();
  });
});
