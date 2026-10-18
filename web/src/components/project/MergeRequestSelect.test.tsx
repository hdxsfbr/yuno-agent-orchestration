// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { MergeRequest } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, project } from "@/test/fixtures";
import QuickRunDialog from "../QuickRunDialog";
import MergeRequestSelect from "./MergeRequestSelect";

let api: ReturnType<typeof installFakeApi>;
let records: MergeRequest[];
let onChange: ReturnType<typeof vi.fn<(id: string) => void>>;

beforeEach(() => {
  records = [
    mergeRequest({ id: "mr1", number: 12, title: "Add the health endpoint" }),
    mergeRequest({ id: "mr2", number: 9, title: "Fix the build" }),
    mergeRequest({ id: "mr3", number: 7, title: "Already merged", state: "merged" }),
    mergeRequest({ id: "mr4", number: 5, title: "Closed unmerged", state: "closed" }),
    mergeRequest({ id: "mr5", project_id: "p2", number: 3, title: "Other project's request" }),
  ];
  onChange = vi.fn<(id: string) => void>();
  api = installFakeApi();
  // Like the real list: narrowed to the project and the state when the query names them.
  api.on("GET", /^\/api\/merge-requests\?/, ({ url }) => {
    const query = new URL(url, "http://yuno.test").searchParams;
    return records.filter((r) => (!query.get("project_id") || r.project_id === query.get("project_id")) && (!query.get("state") || r.state === query.get("state")));
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

type HarnessProps = { projectId: string; initial?: string; errors?: string[]; current?: { id: string; label: string } };

/** Holds the choice the way a form does, and reports each change. */
function Harness({ projectId, initial = "", errors, current }: HarnessProps) {
  const [value, setValue] = useState(initial);
  return (
    <MergeRequestSelect
      projectId={projectId}
      value={value}
      errors={errors}
      current={current}
      onChange={(id) => {
        onChange(id);
        setValue(id);
      }}
    />
  );
}

function renderSelect(props: HarnessProps) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui = (p: HarnessProps) => (
    <QueryClientProvider client={client}>
      <Harness {...p} />
    </QueryClientProvider>
  );
  const view = render(ui(props));
  return { rerender: (p: HarnessProps) => view.rerender(ui(p)) };
}

const select = async () => (await screen.findByLabelText("Merge request")) as HTMLSelectElement;
const labels = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.textContent);

describe("before a project is chosen", () => {
  it("shows nothing and asks for nothing", async () => {
    renderSelect({ projectId: "" });
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByLabelText("Merge request")).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
    expect(api.made("GET", /merge-requests/)).toHaveLength(0);
  });
});

describe("the options", () => {
  it("offers None: open a new one first and selected, then one option per open request as !number title", async () => {
    renderSelect({ projectId: "p1" });
    const s = await select();
    expect(labels(s)).toEqual(["None: open a new one", "!12 Add the health endpoint", "!9 Fix the build"]);
    expect(Array.from(s.options).map((o) => o.value)).toEqual(["", "mr1", "mr2"]);
    expect(s.value).toBe("");
    expect(s.parentElement?.textContent).not.toContain("*");
  });

  it("asks for the open requests of the chosen project only, and lists no merged or closed one", async () => {
    renderSelect({ projectId: "p1" });
    await select();
    expect(api.made("GET", /merge-requests/).map((c) => c.url)).toEqual(["/api/merge-requests?project_id=p1&state=open&limit=200"]);
    expect(screen.queryByText(/Already merged/)).toBeNull();
    expect(screen.queryByText(/Closed unmerged/)).toBeNull();
    expect(screen.queryByText(/Other project's request/)).toBeNull();
  });

  it("leaves out a merged, closed or foreign record even when the server sends one", async () => {
    api.on("GET", /^\/api\/merge-requests\?/, () => records);
    renderSelect({ projectId: "p1" });
    expect(labels(await select())).toEqual(["None: open a new one", "!12 Add the health endpoint", "!9 Fix the build"]);
  });

  it("writes a GitHub request with # and its title", async () => {
    records = [mergeRequest({ id: "mr6", provider: "github", number: 4, title: "Docs", url: "https://github.com/o/r/pull/4" })];
    renderSelect({ projectId: "p1" });
    expect(labels(await select())).toEqual(["None: open a new one", "#4 Docs"]);
  });

  it("offers only None when the project has no open request", async () => {
    records = [];
    renderSelect({ projectId: "p1" });
    expect(labels(await select())).toEqual(["None: open a new one"]);
  });

  it("says what the choice does", async () => {
    renderSelect({ projectId: "p1" });
    expect((await select()).parentElement?.textContent).toContain("The run works on the request's branch and pushes onto it.");
  });
});

describe("choosing", () => {
  it("reports the record's id, and the empty string for None", async () => {
    renderSelect({ projectId: "p1" });
    const s = await select();
    fireEvent.change(s, { target: { value: "mr2" } });
    expect(onChange).toHaveBeenLastCalledWith("mr2");
    expect(s.value).toBe("mr2");
    fireEvent.change(s, { target: { value: "" } });
    expect(onChange).toHaveBeenLastCalledWith("");
  });

  it("follows the project: another project's requests are listed when it changes", async () => {
    const { rerender } = renderSelect({ projectId: "p1" });
    expect(labels(await select())).toContain("!12 Add the health endpoint");
    rerender({ projectId: "p2" });
    await waitFor(() => expect(labels(screen.getByLabelText("Merge request") as HTMLSelectElement)).toEqual(["None: open a new one", "!3 Other project's request"]));
    expect(api.made("GET", /merge-requests/).map((c) => c.url)).toEqual([
      "/api/merge-requests?project_id=p1&state=open&limit=200",
      "/api/merge-requests?project_id=p2&state=open&limit=200",
    ]);
  });

  it("drops a choice that is no longer on offer, so the form never sends an id it does not show", async () => {
    renderSelect({ projectId: "p1", initial: "mr3" });
    const s = await select();
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(""));
    expect(s.value).toBe("");
  });

  it("keeps a choice that is on offer", async () => {
    renderSelect({ projectId: "p1", initial: "mr2" });
    const s = await select();
    expect(s.value).toBe("mr2");
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe("a request a saved schedule names", () => {
  it("stays on offer, marked, when it is no longer open", async () => {
    renderSelect({ projectId: "p1", initial: "mr3", current: { id: "mr3", label: "!7" } });
    const s = await select();
    expect(labels(s)).toEqual(["None: open a new one", "!12 Add the health endpoint", "!9 Fix the build", "!7 (no longer open)"]);
    expect(s.value).toBe("mr3");
    expect(onChange).not.toHaveBeenCalled();
  });

  it("is the ordinary option while it is open", async () => {
    renderSelect({ projectId: "p1", initial: "mr2", current: { id: "mr2", label: "!9" } });
    const s = await select();
    expect(labels(s)).toEqual(["None: open a new one", "!12 Add the health endpoint", "!9 Fix the build"]);
    expect(s.value).toBe("mr2");
  });
});

describe("the server's refusal of the choice", () => {
  it("is shown next to the select, which is marked invalid", async () => {
    renderSelect({ projectId: "p1", initial: "mr1", errors: ["merge request !12 is being updated by run 0192ab"] });
    const s = await select();
    expect(s.getAttribute("aria-invalid")).toBe("true");
    expect(s.parentElement?.textContent).toContain("merge request !12 is being updated by run 0192ab");
  });
});

describe("loading and failing", () => {
  it("says so while the requests load, and shows the refusal meanwhile", async () => {
    let release: (r: MergeRequest[]) => void = () => undefined;
    api.on("GET", /^\/api\/merge-requests\?/, () => new Promise<MergeRequest[]>((r) => (release = r)));
    renderSelect({ projectId: "p1", errors: ["merge request !12 is merged"] });
    expect((await screen.findByRole("status")).textContent).toBe("Loading merge requests…");
    expect(screen.getByText("merge request !12 is merged")).not.toBeNull();
    expect(screen.queryByLabelText("Merge request")).toBeNull();
    release([records[0]]);
    expect(labels(await select())).toEqual(["None: open a new one", "!12 Add the health endpoint"]);
  });

  it("says when the requests cannot be loaded, offers Retry, and shows the refusal beside it", async () => {
    api.on("GET", /^\/api\/merge-requests\?/, () => apiError(500, "internal", "boom"));
    renderSelect({ projectId: "p1", errors: ["merge_request_id is not known"] });
    expect(await screen.findByText(/Could not load the merge requests: boom/)).not.toBeNull();
    expect(screen.getByText("merge_request_id is not known")).not.toBeNull();
    expect(screen.queryByLabelText("Merge request")).toBeNull();

    api.on("GET", /^\/api\/merge-requests\?/, () => [records[0]]);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(labels(await select())).toEqual(["None: open a new one", "!12 Add the health endpoint"]);
  });
});

describe("in a form that starts a run", () => {
  function renderDialog(answer: () => unknown = () => ({ id: "run1" })) {
    api.on("GET", "/api/projects", () => [project({})]);
    api.on("POST", "/api/runs", answer);
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
    fireEvent.change(screen.getByLabelText("Task"), { target: { value: "fix the review comments" } });
  }

  it("is left out of the request body while None is chosen", async () => {
    renderDialog();
    fireEvent.change(await screen.findByLabelText("Project"), { target: { value: "p1" } });
    expect((await select()).value).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the review comments", project_id: "p1" });
  });

  it("is named in the request body by the record's id once chosen", async () => {
    renderDialog();
    fireEvent.change(await screen.findByLabelText("Project"), { target: { value: "p1" } });
    fireEvent.change(await select(), { target: { value: "mr2" } });
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));
    expect(await screen.findByText("a run")).not.toBeNull();
    expect(api.made("POST", "/api/runs")[0].body).toEqual({ agent_id: "a1", task: "fix the review comments", project_id: "p1", merge_request_id: "mr2" });
  });

  it("shows the server's 422 detail on merge_request_id beside the select", async () => {
    renderDialog(() => apiError(422, "validation_failed", "validation failed", [{ path: "merge_request_id", message: "merge request !9 is closed" }]));
    fireEvent.change(await screen.findByLabelText("Project"), { target: { value: "p1" } });
    fireEvent.change(await select(), { target: { value: "mr2" } });
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));
    await waitFor(() => expect(screen.getByLabelText("Merge request").parentElement?.textContent).toContain("merge request !9 is closed"));
    expect(screen.getByLabelText("Merge request").getAttribute("aria-invalid")).toBe("true");
  });
});
