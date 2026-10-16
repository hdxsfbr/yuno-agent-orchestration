// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Skill, SkillInput } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import SkillsPage from "./SkillsPage";

const skill = (over: Partial<Skill>): Skill => ({
  id: "s1",
  name: "code-review",
  description: "Reviews diffs for bugs",
  body: "# Review\n\nRead the diff first.",
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T10:30:00Z",
  ...over,
});

let skills: Skill[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  skills = [skill({}), skill({ id: "s2", name: "release-notes", description: "", updated_at: "2026-10-15T11:00:00Z" })];
  api = installFakeApi();
  api.on("GET", "/api/skills", () => skills);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <SkillsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const dialog = async () => within(await screen.findByRole("dialog"));
const openNew = async () => {
  fireEvent.click(await screen.findByRole("button", { name: "New skill" }));
  return dialog();
};

describe("SkillsPage list", () => {
  it("shows each skill's name, description and last update", async () => {
    renderPage();
    const first = (await screen.findByRole("button", { name: "code-review" })).closest("tr") as HTMLElement;
    expect(within(first).getByText("Reviews diffs for bugs")).not.toBeNull();
    expect(within(first).getByText(formatDateTime("2026-10-15T10:30:00Z"))).not.toBeNull();
    const second = screen.getByRole("button", { name: "release-notes" }).closest("tr") as HTMLElement;
    expect(within(second).getByText("—")).not.toBeNull();
  });

  it("says so when there are none", async () => {
    skills = [];
    renderPage();
    expect(await screen.findByText(/No skills yet/)).not.toBeNull();
  });
});

describe("creating a skill", () => {
  it("sends the name, description and instructions", async () => {
    api.on("POST", "/api/skills", (r) => skill({ id: "s9", ...(r.body as SkillInput) }));
    renderPage();
    const d = await openNew();
    expect(d.getByText("Lowercase words separated by hyphens, such as code-review.")).not.toBeNull();
    fireEvent.change(d.getByLabelText(/^Name/), { target: { value: " incident-triage " } });
    fireEvent.change(d.getByLabelText("Description"), { target: { value: " First steps in an incident " } });
    fireEvent.change(d.getByLabelText(/^Instructions/), { target: { value: "# Triage\n\n1. Check the dashboard" } });
    fireEvent.click(d.getByRole("button", { name: "Create skill" }));

    await waitFor(() => expect(api.made("POST", "/api/skills")).toHaveLength(1));
    expect(api.made("POST", "/api/skills")[0].body).toEqual({ name: "incident-triage", description: "First steps in an incident", body: "# Triage\n\n1. Check the dashboard" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("rejects a name that breaks the pattern and an empty body before sending", async () => {
    renderPage();
    const d = await openNew();
    fireEvent.change(d.getByLabelText(/^Name/), { target: { value: "Bad Name" } });
    fireEvent.click(d.getByRole("button", { name: "Create skill" }));
    const name = d.getByLabelText(/^Name/);
    await waitFor(() => expect(name.getAttribute("aria-invalid")).toBe("true"));
    expect(name.parentElement?.textContent).toContain("Lowercase words separated by hyphens");
    expect(d.getByLabelText(/^Instructions/).parentElement?.textContent).toContain("Write the instructions");
    expect(api.made("POST", "/api/skills")).toHaveLength(0);
  });

  it("puts the API's 422 next to the field it names", async () => {
    api.on("POST", "/api/skills", () =>
      apiError(422, "validation_failed", "validation failed", [
        { path: "name", message: "a skill named code-review already exists" },
        { path: "body", message: "body must be at most 20000 characters" },
      ]),
    );
    renderPage();
    const d = await openNew();
    fireEvent.change(d.getByLabelText(/^Name/), { target: { value: "code-review" } });
    fireEvent.change(d.getByLabelText(/^Instructions/), { target: { value: "x" } });
    fireEvent.click(d.getByRole("button", { name: "Create skill" }));
    await waitFor(() => expect(d.getByLabelText(/^Name/).parentElement?.textContent).toContain("already exists"));
    expect(d.getByLabelText(/^Instructions/).parentElement?.textContent).toContain("at most 20000");
    expect(d.getByLabelText("Description").getAttribute("aria-invalid")).toBeNull();
    expect(screen.getByRole("dialog")).not.toBeNull();
  });
});

describe("editing a skill", () => {
  it("opens on the saved text with the name locked and saves to the same skill", async () => {
    api.on("PUT", /^\/api\/skills\/([^/]+)$/, (r) => skill({ ...(r.body as SkillInput) }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Edit code-review" }));
    const d = await dialog();
    const name = d.getByLabelText(/^Name/) as HTMLInputElement;
    expect(name.value).toBe("code-review");
    expect(name.disabled).toBe(true);
    expect((d.getByLabelText(/^Instructions/) as HTMLTextAreaElement).value).toBe("# Review\n\nRead the diff first.");

    fireEvent.change(d.getByLabelText(/^Instructions/), { target: { value: "# Review\n\nRun the tests too." } });
    fireEvent.click(d.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.made("PUT", "/api/skills/s1")).toHaveLength(1));
    expect(api.made("PUT", "/api/skills/s1")[0].body).toEqual({ name: "code-review", description: "Reviews diffs for bugs", body: "# Review\n\nRun the tests too." });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("opens from the skill's name too", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "release-notes" }));
    expect((await dialog()).getByRole("heading", { name: "Edit release-notes" })).not.toBeNull();
  });
});

describe("deleting a skill", () => {
  it("asks first, then removes it", async () => {
    api.on("DELETE", /^\/api\/skills\/([^/]+)$/, (r) => {
      skills = skills.filter((s) => s.id !== r.match[1]);
      return new Response(null, { status: 204 });
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete code-review" }));
    let d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(api.made("DELETE", /skills/)).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Delete code-review" }));
    d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete skill" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Delete code-review" })).toBeNull());
    expect(api.made("DELETE", "/api/skills/s1")).toHaveLength(1);
  });

  it("shows why the API refused", async () => {
    api.on("DELETE", /^\/api\/skills\/[^/]+$/, () => apiError(409, "conflict", "skill is used by 2 agents"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete code-review" }));
    const d = await dialog();
    fireEvent.click(d.getByRole("button", { name: "Delete skill" }));
    expect((await d.findByRole("alert")).textContent).toContain("skill is used by 2 agents");
    expect(screen.getByRole("button", { name: "Edit code-review" })).not.toBeNull();
  });
});
