// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Project, ProjectInput, Workflow } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { intake, project, workflow } from "@/test/fixtures";
import ProjectDialog from "./ProjectDialog";

const TOKEN = "glpat-not-a-real-token-00";
const LABEL_HINT = (label: string) => `Issues carrying this label become runs. The platform moves it to ${label}:running, ${label}:done or ${label}:failed as the work goes.`;
const BUDGET_SENTENCE = "Set a daily or monthly budget before enabling intake.";

let workflows: Workflow[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  workflows = [
    workflow({ id: "w1", name: "Dev pipeline (ours)" }),
    workflow({ id: "w2", name: "Research" }),
    workflow({ id: "w3", name: "Dev pipeline", template_key: "dev_pipeline" }),
  ];
  api = installFakeApi();
  api.on("GET", "/api/workflows", () => workflows);
  api.on("POST", "/api/projects", (r) => project({ id: "p9", ...(r.body as ProjectInput) } as Partial<Project>));
  api.on("PUT", /^\/api\/projects\/([^/]+)$/, () => project());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderDialog(existing?: Project) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ProjectDialog project={existing} onClose={() => undefined} />
    </QueryClientProvider>,
  );
  return within(screen.getByRole("dialog"));
}

type Dialog = ReturnType<typeof within>;
const section = (d: Dialog) => d.getByRole("group", { name: "Issue intake" });
const enabled = (d: Dialog) => d.getByRole("switch", { name: "Enabled" }) as HTMLInputElement;
const label = (d: Dialog) => d.getByLabelText("Label") as HTMLInputElement;
const workflowSelect = async (d: Dialog) => (await d.findByLabelText(/^Workflow/)) as HTMLSelectElement;
const fill = (d: Dialog, field: RegExp | string, value: string) => fireEvent.change(d.getByLabelText(field), { target: { value } });
const fillNew = (d: Dialog) => {
  fill(d, /^Name/, "api-server");
  fill(d, /^Repository URL/, "https://gitlab.com/acme/api");
  fill(d, /^Access token/, TOKEN);
};
const create = (d: Dialog) => fireEvent.click(d.getByRole("button", { name: "Create project" }));
const save = (d: Dialog) => fireEvent.click(d.getByRole("button", { name: "Save changes" }));
const created = () => api.made("POST", "/api/projects")[0].body as ProjectInput;
const updated = () => api.made("PUT", "/api/projects/p1")[0].body as ProjectInput;

describe("the Issue intake section of a new project", () => {
  it("groups an Enabled switch, a Label field and a Workflow select under the Issue intake label", async () => {
    const d = renderDialog();
    const group = within(section(d));
    expect(enabled(d).checked).toBe(false);
    expect(group.getByRole("switch", { name: "Enabled" })).not.toBeNull();
    expect(label(d).value).toBe("yuno");
    expect(group.getByText(LABEL_HINT("yuno"))).not.toBeNull();
    const select = await workflowSelect(d);
    expect(within(section(d)).getByLabelText(/^Workflow/)).toBe(select);
    expect(select.value).toBe("");
  });

  it("offers Choose a workflow and the operator's workflows, never a shipped template", async () => {
    const d = renderDialog();
    const select = await workflowSelect(d);
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["Choose a workflow", "Dev pipeline (ours)", "Research"]);
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "w1", "w2"]);
  });

  it("marks the workflow as required only once the intake is enabled", async () => {
    const d = renderDialog();
    const select = await workflowSelect(d);
    expect(select.parentElement?.textContent).not.toContain("*");
    fireEvent.click(enabled(d));
    expect(select.parentElement?.textContent).toContain("*");
  });

  it("writes the label that is typed into the hint", async () => {
    const d = renderDialog();
    fill(d, "Label", "robot");
    expect(within(section(d)).getByText(LABEL_HINT("robot"))).not.toBeNull();
    expect(within(section(d)).queryByText(LABEL_HINT("yuno"))).toBeNull();
  });

  it("sends no intake while the section is left alone", async () => {
    const d = renderDialog();
    await workflowSelect(d);
    fillNew(d);
    create(d);
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(created()).not.toHaveProperty("intake");
  });

  it("sends the settings that were entered, with the label trimmed", async () => {
    const d = renderDialog();
    fillNew(d);
    fill(d, "Per day (USD)", "5");
    fireEvent.click(enabled(d));
    fill(d, "Label", " robot ");
    fireEvent.change(await workflowSelect(d), { target: { value: "w2" } });
    create(d);
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(created().intake).toEqual({ enabled: true, label: "robot", workflow_id: "w2" });
  });

  it("sends the settings of a section that was opened but left off, without a workflow", async () => {
    const d = renderDialog();
    fillNew(d);
    fill(d, "Label", "robot");
    create(d);
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(created().intake).toEqual({ enabled: false, label: "robot" });
  });
});

describe("the budget sentence", () => {
  const sentence = (d: Dialog) => d.queryByText(BUDGET_SENTENCE);

  it("appears under the switch when the intake is being enabled and the form has no cap", async () => {
    const d = renderDialog();
    expect(sentence(d)).toBeNull();
    fireEvent.click(enabled(d));
    const text = sentence(d);
    expect(text).not.toBeNull();
    expect(enabled(d).compareDocumentPosition(text!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(label(d).compareDocumentPosition(text!) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
  });

  it("goes away when a cap is entered, comes back when it is emptied, and goes with the switch", async () => {
    const d = renderDialog();
    fireEvent.click(enabled(d));
    fill(d, "Per day (USD)", "5");
    expect(sentence(d)).toBeNull();
    fill(d, "Per day (USD)", "");
    expect(sentence(d)).not.toBeNull();
    fill(d, "Per month (USD)", "100");
    expect(sentence(d)).toBeNull();
    fill(d, "Per month (USD)", "");
    fireEvent.click(enabled(d));
    expect(sentence(d)).toBeNull();
  });

  it("is not shown for a project that already has a cap", async () => {
    const d = renderDialog(project({ max_cost_usd_per_month: 100, intake: intake({ enabled: true }) }));
    expect(enabled(d).checked).toBe(true);
    expect(sentence(d)).toBeNull();
  });

  it("is shown for a project with the intake on and no cap", async () => {
    const d = renderDialog(project({ intake: intake({ enabled: true }) }));
    expect(sentence(d)).not.toBeNull();
  });
});

describe("the Issue intake section of a project that is edited", () => {
  const saved = () => project({ max_cost_usd_per_day: 5, intake: intake({ enabled: true, label: "robot", workflow_id: "w2", workflow_name: "Research" }) });

  it("opens on the stored settings", async () => {
    const d = renderDialog(saved());
    expect(enabled(d).checked).toBe(true);
    expect(label(d).value).toBe("robot");
    expect(within(section(d)).getByText(LABEL_HINT("robot"))).not.toBeNull();
    expect((await workflowSelect(d)).value).toBe("w2");
  });

  it("opens on the defaults for a project that never had settings, and for one the API sent none for", async () => {
    const d = renderDialog(project({ intake: intake({ enabled: false, label: "yuno", workflow_id: null, workflow_name: null }) }));
    expect(enabled(d).checked).toBe(false);
    expect(label(d).value).toBe("yuno");
    expect((await workflowSelect(d)).value).toBe("");
    cleanup();
    const bare = renderDialog(project());
    expect(enabled(bare).checked).toBe(false);
    expect(label(bare).value).toBe("yuno");
  });

  it("keeps the stored settings by sending none when the section is not touched", async () => {
    const d = renderDialog(saved());
    expect((await workflowSelect(d)).value).toBe("w2");
    fill(d, "Description", "A new description");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated()).not.toHaveProperty("intake");
    expect(updated().description).toBe("A new description");
  });

  it("sends all three settings once one of them is touched", async () => {
    const d = renderDialog(saved());
    expect((await workflowSelect(d)).value).toBe("w2");
    fill(d, "Label", "robot-2");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated().intake).toEqual({ enabled: true, label: "robot-2", workflow_id: "w2" });
  });

  it("switches the intake off with the label and the workflow it had", async () => {
    const d = renderDialog(saved());
    expect((await workflowSelect(d)).value).toBe("w2");
    fireEvent.click(enabled(d));
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated().intake).toEqual({ enabled: false, label: "robot", workflow_id: "w2" });
  });

  it("switches to another workflow", async () => {
    const d = renderDialog(saved());
    fireEvent.change(await workflowSelect(d), { target: { value: "w1" } });
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated().intake).toEqual({ enabled: true, label: "robot", workflow_id: "w1" });
  });

  it("does not hold back a save of something else for a stored intake that is on without a workflow", async () => {
    const d = renderDialog(project({ max_cost_usd_per_day: 5, intake: intake({ enabled: true, workflow_id: null, workflow_name: null, last_error: "intake: no workflow" }) }));
    await workflowSelect(d);
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated()).not.toHaveProperty("intake");
  });

  it("does not take a workflow that has been deleted since for a chosen one", async () => {
    workflows = workflows.filter((w) => w.id !== "w2");
    const d = renderDialog(saved());
    expect((await workflowSelect(d)).value).toBe("");
    fill(d, "Label", "robot-2");
    save(d);
    expect(await d.findByText("Choose a workflow", { selector: "p" })).not.toBeNull();
    expect(api.made("PUT", "/api/projects/p1")).toHaveLength(0);
  });
});

describe("checking the section before sending", () => {
  it("asks for a workflow when the intake is enabled without one, and sends nothing", async () => {
    const d = renderDialog();
    fillNew(d);
    fill(d, "Per day (USD)", "5");
    fireEvent.click(enabled(d));
    await workflowSelect(d);
    create(d);
    expect(await d.findByText("Choose a workflow", { selector: "p" })).not.toBeNull();
    expect((await workflowSelect(d)).getAttribute("aria-invalid")).toBe("true");
    expect(api.made("POST", "/api/projects")).toHaveLength(0);
  });

  it.each([
    ["empty", ""],
    ["blank", "   "],
    ["with a space", "my label"],
    ["with a comma", "a,b"],
    ["longer than 50 characters", "x".repeat(51)],
  ])("refuses a label that is %s", async (_, text) => {
    const d = renderDialog();
    fillNew(d);
    fill(d, "Label", text);
    create(d);
    await waitFor(() => expect(label(d).getAttribute("aria-invalid")).toBe("true"));
    expect(label(d).parentElement?.textContent).toContain("1 to 50 characters without spaces or commas");
    expect(api.made("POST", "/api/projects")).toHaveLength(0);
  });

  it("accepts a label of exactly 50 characters", async () => {
    const d = renderDialog();
    fillNew(d);
    fill(d, "Label", "x".repeat(50));
    create(d);
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(created().intake?.label).toHaveLength(50);
  });

  it("lets the server decide whether a budget is needed", async () => {
    const d = renderDialog();
    fillNew(d);
    fireEvent.click(enabled(d));
    fireEvent.change(await workflowSelect(d), { target: { value: "w1" } });
    create(d);
    await waitFor(() => expect(api.made("POST", "/api/projects")).toHaveLength(1));
    expect(created().intake).toEqual({ enabled: true, label: "yuno", workflow_id: "w1" });
  });
});

describe("the server's refusals of the section", () => {
  it("are shown next to the label, the workflow and the switch they name", async () => {
    api.on("POST", "/api/projects", () =>
      apiError(422, "validation_failed", "validation failed", [
        { path: "intake.label", message: "1 to 50 characters without spaces or commas" },
        { path: "intake.workflow_id", message: "the workflow does not use a project" },
        { path: "intake.enabled", message: "set a daily or monthly budget before enabling intake" },
      ]),
    );
    const d = renderDialog();
    fillNew(d);
    fireEvent.click(enabled(d));
    fireEvent.change(await workflowSelect(d), { target: { value: "w2" } });
    create(d);
    await waitFor(() => expect(label(d).parentElement?.textContent).toContain("1 to 50 characters without spaces or commas"));
    expect((await workflowSelect(d)).parentElement?.textContent).toContain("the workflow does not use a project");
    expect(enabled(d).closest("div")?.parentElement?.textContent).toContain("set a daily or monthly budget before enabling intake");
    expect(enabled(d).getAttribute("aria-invalid")).toBe("true");
    expect(label(d).getAttribute("aria-invalid")).toBe("true");
    expect((await workflowSelect(d)).getAttribute("aria-invalid")).toBe("true");
    // none of them is listed a second time under the form
    expect(d.queryByText(/^intake\./)).toBeNull();
    expect(screen.getByRole("dialog")).not.toBeNull();
  });

  it("lists a refusal of the section as a whole under the form", async () => {
    api.on("POST", "/api/projects", () => apiError(422, "validation_failed", "validation failed", [{ path: "intake", message: "unknown field" }]));
    const d = renderDialog();
    fillNew(d);
    fireEvent.click(enabled(d));
    fireEvent.click(enabled(d));
    create(d);
    expect(await d.findByText("intake: unknown field")).not.toBeNull();
  });
});

describe("when the workflows cannot be loaded", () => {
  beforeEach(() => {
    api.on("GET", "/api/workflows", () => apiError(500, "internal", "boom"));
  });

  it("says so, with Retry, and the rest of the form still saves", async () => {
    const d = renderDialog(project({ intake: intake({ enabled: false, workflow_id: null, workflow_name: null }) }));
    expect(await d.findByText(/Could not load the workflows: boom/)).not.toBeNull();
    expect(d.queryByLabelText(/^Workflow/)).toBeNull();
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated()).not.toHaveProperty("intake");

    api.on("GET", "/api/workflows", () => workflows);
    fireEvent.click(d.getByRole("button", { name: "Retry" }));
    expect((await workflowSelect(d)).options).toHaveLength(3);
  });

  it("keeps the stored workflow when only the label is changed", async () => {
    const d = renderDialog(project({ max_cost_usd_per_day: 5, intake: intake({ enabled: true, label: "robot", workflow_id: "w2" }) }));
    await d.findByText(/Could not load the workflows/);
    fill(d, "Label", "robot-2");
    save(d);
    await waitFor(() => expect(api.made("PUT", "/api/projects/p1")).toHaveLength(1));
    expect(updated().intake).toEqual({ enabled: true, label: "robot-2", workflow_id: "w2" });
  });
});
