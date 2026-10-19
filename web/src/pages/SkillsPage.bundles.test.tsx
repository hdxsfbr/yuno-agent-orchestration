// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { Skill, SkillFile } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import SkillsPage from "./SkillsPage";

const skill = (over: Partial<Skill>): Skill => ({
  id: "s1",
  name: "code-review",
  description: "Reviews diffs",
  body: "x",
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T10:30:00Z",
  ...over,
});

let skills: Skill[];
let tree: SkillFile[];
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  skills = [
    skill({}),
    skill({ id: "s2", name: "pdf", description: "PDF tools", source: "library", source_url: "https://github.com/anthropics/skills", license: "Apache-2.0", files_count: 3 }),
    skill({ id: "s3", name: "from-git", source: "git", source_url: "https://github.com/o/r", source_ref: "0123456789abcdef", files_count: 1 }),
  ];
  tree = [
    { path: "scripts/run.sh", size: 120, mode: 493 },
    { path: "notes.md", size: 40, mode: 420 },
    { path: "logo.png", size: 2048, mode: 420 },
  ];
  api = installFakeApi();
  api.on("GET", "/api/skills", () => skills);
  api.on("GET", "/api/skills/s2/files", () => tree);
  api.on("GET", "/api/skills/s2/files/notes.md", () => ({ path: "notes.md", size: 40, mode: 420, content: "hello" }));
  api.on("GET", "/api/skills/s2/files/scripts%2Frun.sh", () => ({ path: "scripts/run.sh", size: 120, mode: 493, content: "#!/bin/sh\necho hi" }));
  api.on("GET", "/api/skills/s2/files/logo.png", () => apiError(415, "unsupported_media_type", "binary file"));
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
const rowOf = async (name: string) => within((await screen.findByRole("button", { name })).closest("tr") as HTMLElement);

describe("source, licence and file count", () => {
  it("shows a badge, the licence and the file count per skill", async () => {
    renderPage();
    const lib = await rowOf("pdf");
    expect(lib.getByText("Library")).not.toBeNull();
    expect(lib.getByText("Apache-2.0")).not.toBeNull();
    expect(lib.getByText("3")).not.toBeNull();
    expect((await rowOf("from-git")).getByText("Git")).not.toBeNull();
    expect((await rowOf("code-review")).getByText("Custom")).not.toBeNull();
  });

  it("filters by source", async () => {
    renderPage();
    await screen.findByRole("button", { name: "pdf" });
    fireEvent.change(screen.getByLabelText("Source"), { target: { value: "library" } });
    expect(screen.getByRole("button", { name: "pdf" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: "code-review" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Source"), { target: { value: "upload" } });
    expect(screen.getByText(/No skills come from this source/)).not.toBeNull();
  });
});

describe("Upload skill", () => {
  const choose = async (name = "pdf.zip") => {
    fireEvent.click(await screen.findByRole("button", { name: "Upload skill" }));
    const d = await dialog();
    const input = d.getByLabelText("Skill archive") as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["zip"], name, { type: "application/zip" })] } });
    return d;
  };

  it("sends the zip as multipart and reports success", async () => {
    api.on("POST", "/api/skills/import", () => skill({ id: "s9", name: "pdf-tools", source: "upload" }));
    renderPage();
    const d = await choose();
    expect(d.getByText(/zip it first/)).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Upload skill" }));
    await waitFor(() => expect(api.made("POST", "/api/skills/import")).toHaveLength(1));
    const init = vi.mocked(fetch).mock.calls.find((c) => String(c[0]) === "/api/skills/import")?.[1];
    expect(init?.body).toBeInstanceOf(FormData);
    expect((init?.body as FormData).get("file")).toBeInstanceOf(File);
    expect(new Headers(init?.headers).get("content-type")).toBeNull();
    expect((await screen.findByRole("status")).textContent).toContain("pdf-tools");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("offers to replace after a 409 and retries with replace=true", async () => {
    api.on("POST", "/api/skills/import", () => apiError(409, "conflict", "skill pdf already exists"));
    renderPage();
    const d = await choose();
    fireEvent.click(d.getByRole("button", { name: "Upload skill" }));
    expect((await d.findByRole("alert")).textContent).toContain("skill pdf already exists");
    expect(d.getByText(/Tick this box/)).not.toBeNull();

    api.on("POST", "/api/skills/import?replace=true", () => skill({ id: "s2", name: "pdf", source: "upload" }));
    fireEvent.click(d.getByLabelText(/Replace the existing skill/));
    fireEvent.click(d.getByRole("button", { name: "Upload skill" }));
    await waitFor(() => expect(api.made("POST", "/api/skills/import?replace=true")).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("Import from Git", () => {
  it("sends the address, ref and path", async () => {
    api.on("POST", "/api/skills/import-git", () => skill({ id: "s8", name: "review", source: "git" }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Import from Git" }));
    const d = await dialog();
    fireEvent.change(d.getByLabelText(/^Repository address/), { target: { value: " https://github.com/o/r " } });
    fireEvent.change(d.getByLabelText("Branch, tag or commit"), { target: { value: "v1" } });
    fireEvent.change(d.getByLabelText("Folder in the repository"), { target: { value: "skills/review" } });
    fireEvent.click(d.getByRole("button", { name: "Import skill" }));
    await waitFor(() => expect(api.made("POST", "/api/skills/import-git")).toHaveLength(1));
    expect(api.made("POST", "/api/skills/import-git")[0].body).toEqual({ url: "https://github.com/o/r", ref: "v1", path: "skills/review" });
    expect((await screen.findByRole("status")).textContent).toContain("review");
  });

  it.each([
    [400, "host example.com is not allowed"],
    [413, "skill is over the 20 MiB cap"],
    [502, "clone failed: repository not found"],
  ])("shows a %i from the server verbatim", async (status, message) => {
    api.on("POST", "/api/skills/import-git", () => apiError(status, "import_failed", message));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Import from Git" }));
    const d = await dialog();
    fireEvent.change(d.getByLabelText(/^Repository address/), { target: { value: "https://example.com/o/r" } });
    fireEvent.click(d.getByRole("button", { name: "Import skill" }));
    expect((await d.findByRole("alert")).textContent).toBe(message);
    expect(screen.getByRole("dialog")).not.toBeNull();
  });
});

describe("import refusals and partial saves", () => {
  const openGit = async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Import from Git" }));
    const d = await dialog();
    fireEvent.change(d.getByLabelText(/^Repository address/), { target: { value: "https://example.com/o/r" } });
    return d;
  };

  it("shows a 422 with the server's reasons, each once", async () => {
    api.on("POST", "/api/skills/import-git", () =>
      apiError(422, "invalid_skill", "invalid skill: name must be lowercase", [
        { path: "name", message: "name must be lowercase" },
        { path: "description", message: "description is required" },
        { path: "description", message: "description is required" },
      ]),
    );
    const d = await openGit();
    fireEvent.click(d.getByRole("button", { name: "Import skill" }));
    const text = (await d.findByRole("alert")).textContent ?? "";
    expect(text).toBe("invalid skill: name must be lowercase: description is required");
  });

  it("treats a 502 materialize_failed as saved and closes", async () => {
    api.on("POST", "/api/skills/import-git", () => apiError(502, "materialize_failed", "runtime unreachable"));
    const d = await openGit();
    fireEvent.click(d.getByRole("button", { name: "Import skill" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect((await screen.findByRole("status")).textContent).toBe("Saved. It will apply when the runtime is reachable.");
  });
});

describe("skill files", () => {
  const openFiles = async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Files of pdf" }));
    return dialog();
  };

  it("shows the tree with sizes, an executable mark, the source and the export link", async () => {
    const d = await openFiles();
    const list = within(await d.findByRole("list", { name: "Files" }));
    expect(list.getByText("scripts/")).not.toBeNull();
    const run = list.getByRole("button", { name: /run\.sh/ });
    expect(run.textContent).toContain("exec");
    expect(run.textContent).toContain("120 B");
    expect(list.getByRole("button", { name: /logo\.png/ }).textContent).toContain("2 KiB");
    expect(d.getByRole("link", { name: "Export zip" }).getAttribute("href")).toBe("/api/skills/s2/export");
    expect(d.getByText(/Licence Apache-2.0/)).not.toBeNull();
    expect(d.getByRole("link", { name: "https://github.com/anthropics/skills" })).not.toBeNull();
  });

  it("opens a text file and saves it", async () => {
    api.on("PUT", "/api/skills/s2/files/notes.md", (r) => r.body);
    const d = await openFiles();
    fireEvent.click(await d.findByRole("button", { name: /notes\.md/ }));
    const box = (await d.findByLabelText("Content of notes.md")) as HTMLTextAreaElement;
    expect(box.value).toBe("hello");
    fireEvent.change(box, { target: { value: "hello again" } });
    fireEvent.click(d.getByRole("button", { name: "Save file" }));
    await waitFor(() => expect(api.made("PUT", "/api/skills/s2/files/notes.md")).toHaveLength(1));
    expect(api.made("PUT", "/api/skills/s2/files/notes.md")[0].body).toMatchObject({ path: "notes.md", content: "hello again" });
    expect((await d.findByRole("status")).textContent).toBe("Saved.");
  });

  it("says a binary file is binary, with its size", async () => {
    const d = await openFiles();
    fireEvent.click(await d.findByRole("button", { name: /logo\.png/ }));
    expect(await d.findByText(/binary file/)).not.toBeNull();
    expect(d.queryByRole("textbox")).toBeNull();
  });

  it("deletes a file after a confirmation", async () => {
    api.on("DELETE", "/api/skills/s2/files/notes.md", () => new Response(null, { status: 204 }));
    const d = await openFiles();
    fireEvent.click(await d.findByRole("button", { name: /notes\.md/ }));
    await d.findByLabelText("Content of notes.md");
    fireEvent.click(d.getByRole("button", { name: "Delete file" }));
    const confirm = within((await screen.findAllByRole("dialog")).at(-1) as HTMLElement);
    fireEvent.click(confirm.getByRole("button", { name: "Delete file" }));
    await waitFor(() => expect(api.made("DELETE", "/api/skills/s2/files/notes.md")).toHaveLength(1));
  });

  it("does not drop unsaved text without asking", async () => {
    const d = await openFiles();
    fireEvent.click(await d.findByRole("button", { name: /notes\.md/ }));
    fireEvent.change(await d.findByLabelText("Content of notes.md"), { target: { value: "draft" } });
    fireEvent.click(d.getByRole("button", { name: /run\.sh/ }));
    expect((await screen.findAllByRole("dialog")).length).toBe(2);
    expect(screen.getByText(/unsaved changes in notes\.md/)).not.toBeNull();
  });
});
