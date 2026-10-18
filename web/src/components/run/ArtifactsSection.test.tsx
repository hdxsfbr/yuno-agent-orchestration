// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Artifact, RunStatus } from "@/lib/api";
import { clearToken, setToken } from "@/lib/auth";
import { formatDateTime } from "@/lib/format";
import { artifactsKey } from "@/lib/useRunArtifacts";
import { apiError, installFakeApi } from "@/test/fakeApi";
import ArtifactsSection from "./ArtifactsSection";

const RUN = "r1";
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

let api: ReturnType<typeof installFakeApi>;
let files: Artifact[];
/** What each artifact's download answers: its bytes and the content type the server sends. */
let contents: Record<string, { body: BodyInit; type: string } | Response>;
let created: Blob[];
let revoked: string[];
let clicked: { download: string; href: string }[];

const artifact = (over: Partial<Artifact> = {}): Artifact => ({
  id: "a1",
  run_id: RUN,
  node_id: "writer",
  visit: 1,
  path: "report.md",
  size: 2048,
  sha256: "1".repeat(64),
  content_type: "text/markdown",
  generated: false,
  created_at: ago(10 * 60_000),
  updated_at: ago(5 * 60_000 + 5_000),
  ...over,
});

beforeEach(() => {
  api = installFakeApi();
  files = [artifact()];
  contents = { a1: { body: "# Findings\n\nAll good.\n", type: "text/plain; charset=utf-8" } };
  created = [];
  revoked = [];
  clicked = [];
  api.on("GET", `/api/runs/${RUN}/artifacts`, () => files);
  api.on("GET", /^\/api\/runs\/r1\/artifacts\/([^/]+)$/, (r) => {
    const c = contents[decodeURIComponent(r.match[1])];
    if (!c) return apiError(404, "not_found", "artifact file is missing");
    if (c instanceof Response) return c.clone();
    return new Response(c.body, { status: 200, headers: { "Content-Type": c.type } });
  });
  let n = 0;
  URL.createObjectURL = vi.fn((blob: Blob | MediaSource) => {
    created.push(blob as Blob);
    return `blob:http://localhost/${++n}`;
  });
  URL.revokeObjectURL = vi.fn((url: string) => {
    revoked.push(url);
  });
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push({ download: this.download, href: this.href });
  });
});

afterEach(() => {
  cleanup();
  clearToken();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  delete (URL as unknown as Record<string, unknown>).createObjectURL;
  delete (URL as unknown as Record<string, unknown>).revokeObjectURL;
});

function renderSection(status: RunStatus = "succeeded") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ArtifactsSection runId={RUN} status={status} />
    </QueryClientProvider>,
  );
  return client;
}

const table = () => screen.findByRole("table", { name: "Files delivered by this run" });
const rowOf = async (path: string) => within((await screen.findByRole("rowheader", { name: path })).closest("tr")!);

describe("the table", () => {
  it("lists each file with its size, the step and visit that delivered it, and when it was collected", async () => {
    files = [
      artifact(),
      artifact({ id: "a2", path: "data/rows.csv", size: 1_572_864, node_id: "analyst", visit: 2, content_type: "text/csv", updated_at: ago(3 * 3_600_000 + 1000) }),
      artifact({ id: "a3", path: "diff.patch", size: 512, generated: true, content_type: "text/x-patch", node_id: "coder", visit: 1, updated_at: ago(2_000) }),
    ];
    renderSection();
    const t = within(await table());
    expect(t.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["File", "Size", "Step", "Updated", "Actions"]);
    // one body row per file, in the order the server gave
    expect(t.getAllByRole("rowheader").map((h) => h.textContent)).toEqual(["report.md", "data/rows.csv", "diff.patch"]);

    const report = await rowOf("report.md");
    expect(report.getByText("2 KiB")).not.toBeNull();
    expect(report.getByText("writer").parentElement!.textContent).toBe("writer visit 1");
    expect(report.getByText("5 min ago")).not.toBeNull();
    expect(report.getByText("5 min ago").getAttribute("title")).toBe(formatDateTime(files[0].updated_at));

    const rows = await rowOf("data/rows.csv");
    expect(rows.getByText("1.5 MiB")).not.toBeNull();
    expect(rows.getByText("analyst").parentElement!.textContent).toBe("analyst visit 2");
    expect(rows.getByText("3 h ago")).not.toBeNull();

    const diff = await rowOf("diff.patch");
    expect(diff.getByText("512 B")).not.toBeNull();
    expect(diff.getByText("just now")).not.toBeNull();
  });

  it("is a section called Artifacts with the number of files", async () => {
    files = [artifact(), artifact({ id: "a2", path: "b.txt" })];
    renderSection();
    await table();
    const heading = screen.getByRole("heading", { name: /^Artifacts/ });
    expect(heading.textContent).toBe("Artifacts2");
    expect(heading.closest("section")!.id).toBe("artifacts");
  });

  it("names the file on its buttons", async () => {
    files = [artifact(), artifact({ id: "a2", path: "reports/q3/summary.md" }), artifact({ id: "a3", path: "bundle.zip", content_type: "application/zip" })];
    renderSection();
    await table();
    for (const name of ["Download report.md", "Preview report.md", "Download reports/q3/summary.md", "Preview reports/q3/summary.md", "Download bundle.zip"]) {
      expect(screen.getByRole("button", { name })).not.toBeNull();
    }
    expect(screen.queryByRole("button", { name: "Preview bundle.zip" })).toBeNull();
  });
});

describe("empty and failed", () => {
  it("says no files have been delivered yet while the run is not over", async () => {
    files = [];
    for (const status of ["queued", "running", "waiting_approval"] as const) {
      renderSection(status);
      expect(await screen.findByText("No files delivered yet.")).not.toBeNull();
      expect(screen.queryByRole("table")).toBeNull();
      cleanup();
    }
  });

  it("says no files were delivered once the run is over", async () => {
    files = [];
    for (const status of ["succeeded", "failed", "cancelled", "timed_out"] as const) {
      renderSection(status);
      expect(await screen.findByText("No files were delivered.")).not.toBeNull();
      expect(screen.queryByText("No files delivered yet.")).toBeNull();
      cleanup();
    }
  });

  it("treats a null list as empty", async () => {
    api.on("GET", `/api/runs/${RUN}/artifacts`, () => null);
    renderSection("failed");
    expect(await screen.findByText("No files were delivered.")).not.toBeNull();
    expect(screen.getByRole("heading", { name: "Artifacts" }).textContent).toBe("Artifacts");
  });

  it("shows the inline error when the list cannot be loaded, and loads it again on Retry", async () => {
    let failing = true;
    api.on("GET", `/api/runs/${RUN}/artifacts`, () => (failing ? apiError(500, "internal", "the store is unavailable") : files));
    renderSection();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Could not load the delivered files: the store is unavailable");
    failing = false;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await table()).not.toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("refreshes the list when the query is invalidated, as the run's stream does when files are collected", async () => {
    files = [];
    const client = renderSection("running");
    await screen.findByText("No files delivered yet.");
    files = [artifact()];
    await client.invalidateQueries({ queryKey: artifactsKey(RUN) });
    expect(await screen.findByRole("rowheader", { name: "report.md" })).not.toBeNull();
    expect(screen.queryByText("No files delivered yet.")).toBeNull();
  });
});

describe("Download", () => {
  it("fetches the file with the token and saves it under its base name through an object URL", async () => {
    setToken("tok-1234");
    files = [artifact({ id: "a2", path: "reports/q3/summary.md" })];
    contents = { a2: { body: "numbers", type: "text/plain; charset=utf-8" } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Download reports/q3/summary.md" }));
    await waitFor(() => expect(clicked).toHaveLength(1));

    const get = api.made("GET", `/api/runs/${RUN}/artifacts/a2`);
    expect(get).toHaveLength(1);
    expect(get[0].headers.authorization).toBe("Bearer tok-1234");
    expect(get[0].url).not.toContain("tok-1234");

    expect(created).toHaveLength(1);
    expect(await created[0].text()).toBe("numbers");
    expect(clicked[0]).toEqual({ download: "summary.md", href: "blob:http://localhost/1" });
    await waitFor(() => expect(revoked).toEqual(["blob:http://localhost/1"]));
    // no link is left in the page
    expect(document.querySelector("a[download]")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("downloads a file that cannot be previewed, such as an archive", async () => {
    files = [artifact({ id: "z1", path: "bundle.zip", content_type: "application/zip", size: 5_000_000 })];
    contents = { z1: { body: new Uint8Array([80, 75, 3, 4]), type: "application/zip" } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Download bundle.zip" }));
    await waitFor(() => expect(clicked).toHaveLength(1));
    expect(clicked[0].download).toBe("bundle.zip");
    expect(created[0].size).toBe(4);
  });

  it("says why when the file cannot be downloaded", async () => {
    contents = {};
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Download report.md" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Could not download report.md: artifact file is missing");
    expect(clicked).toHaveLength(0);
    expect(created).toHaveLength(0);
  });

  it("holds the other downloads while one is on its way", async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((r) => (release = r));
    files = [artifact(), artifact({ id: "a2", path: "b.txt", content_type: "text/plain" })];
    api.on("GET", /^\/api\/runs\/r1\/artifacts\/a1$/, async () => {
      await gate;
      return new Response("late", { status: 200, headers: { "Content-Type": "text/plain" } });
    });
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Download report.md" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Download report.md" }).textContent).toBe("Downloading…"));
    expect((screen.getByRole("button", { name: "Download b.txt" }) as HTMLButtonElement).disabled).toBe(true);
    release();
    await waitFor(() => expect(clicked).toHaveLength(1));
    await waitFor(() => expect((screen.getByRole("button", { name: "Download b.txt" }) as HTMLButtonElement).disabled).toBe(false));
  });
});

describe("Preview", () => {
  const previewOf = (path: string) => screen.findByRole("region", { name: `Preview of ${path}` });

  it("shows a text file in a pre, as text and never as markup", async () => {
    contents = { a1: { body: "# Findings <b>bold</b>\n<script>alert(1)</script>\n", type: "text/plain; charset=utf-8" } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview report.md" }));
    const region = within(await previewOf("report.md"));
    const pre = await region.findByText(/Findings/);
    expect(pre.tagName).toBe("PRE");
    expect(pre.textContent).toBe("# Findings <b>bold</b>\n<script>alert(1)</script>\n");
    // the tags are characters, not elements
    expect(pre.querySelector("b, script")).toBeNull();
    expect(document.querySelector("script")).toBeNull();
    expect(created).toHaveLength(0);
    expect(api.made("GET", `/api/runs/${RUN}/artifacts/a1`)).toHaveLength(1);
  });

  it("opens and closes with the button", async () => {
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview report.md" }));
    await screen.findByText(/All good/);
    const hide = screen.getByRole("button", { name: "Hide preview of report.md" });
    expect(hide.getAttribute("aria-expanded")).toBe("true");
    expect(hide.textContent).toBe("Hide");
    expect(screen.queryByRole("button", { name: "Preview report.md" })).toBeNull();
    fireEvent.click(hide);
    expect(screen.queryByRole("region", { name: "Preview of report.md" })).toBeNull();
    expect(screen.queryByText(/All good/)).toBeNull();
    const again = screen.getByRole("button", { name: "Preview report.md" });
    expect(again.getAttribute("aria-expanded")).toBe("false");
  });

  it("shows a patch, JSON and a content type that carries a charset as text", async () => {
    files = [
      artifact({ id: "p1", path: "diff.patch", content_type: "text/x-patch", generated: true }),
      artifact({ id: "j1", path: "result.json", content_type: "application/json" }),
      artifact({ id: "t1", path: "notes.txt", content_type: "text/plain; charset=utf-8" }),
    ];
    contents = {
      p1: { body: "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n", type: "text/plain; charset=utf-8" },
      j1: { body: '{"ok": true}', type: "application/json" },
      t1: { body: "plain notes", type: "text/plain; charset=utf-8" },
    };
    renderSection();
    for (const path of ["diff.patch", "result.json", "notes.txt"]) fireEvent.click(await screen.findByRole("button", { name: `Preview ${path}` }));
    expect((await within(await previewOf("diff.patch")).findByText(/\+new/)).tagName).toBe("PRE");
    expect((await within(await previewOf("result.json")).findByText(/"ok": true/)).tagName).toBe("PRE");
    expect((await within(await previewOf("notes.txt")).findByText("plain notes")).tagName).toBe("PRE");
  });

  it("shows an SVG as its source, never drawn", async () => {
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><circle r="5"/></svg>';
    files = [artifact({ id: "s1", path: "chart.svg", content_type: "image/svg+xml" })];
    contents = { s1: { body: svg, type: "text/plain; charset=utf-8" } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview chart.svg" }));
    const region = within(await previewOf("chart.svg"));
    expect((await region.findByText(/circle/)).textContent).toBe(svg);
    expect(region.queryByRole("img")).toBeNull();
    expect(document.querySelector("svg")).toBeNull();
    expect(created).toHaveLength(0);
  });

  it("shows an image from an object URL with the path as its text, and lets the URL go when it is closed", async () => {
    files = [artifact({ id: "i1", path: "plots/chart.png", content_type: "image/png", size: 4000 })];
    contents = { i1: { body: new Uint8Array([137, 80, 78, 71]), type: "image/png" } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview plots/chart.png" }));
    const region = within(await previewOf("plots/chart.png"));
    const img = (await region.findByRole("img", { name: "plots/chart.png" })) as HTMLImageElement;
    expect(img.getAttribute("src")).toBe("blob:http://localhost/1");
    expect(img.getAttribute("alt")).toBe("plots/chart.png");
    expect(created).toHaveLength(1);
    expect(revoked).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Hide preview of plots/chart.png" }));
    await waitFor(() => expect(revoked).toEqual(["blob:http://localhost/1"]));
  });

  it.each(["image/jpeg", "image/gif", "image/webp"])("previews %s as an image", async (type) => {
    files = [artifact({ id: "i1", path: "pic", content_type: type, size: 100 })];
    contents = { i1: { body: new Uint8Array([1, 2, 3]), type } };
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview pic" }));
    expect(await screen.findByRole("img", { name: "pic" })).not.toBeNull();
  });

  it("offers no Preview for a text file over 256 KiB, a zip or an unknown type, and offers it at exactly 256 KiB", async () => {
    files = [
      artifact({ id: "a1", path: "exact.txt", size: 262_144, content_type: "text/plain" }),
      artifact({ id: "a2", path: "big.txt", size: 262_145, content_type: "text/plain" }),
      artifact({ id: "a3", path: "huge.log", size: 300 * 1024, content_type: "text/plain" }),
      artifact({ id: "a4", path: "bundle.zip", size: 1000, content_type: "application/zip" }),
      artifact({ id: "a5", path: "blob.bin", size: 1000, content_type: "application/octet-stream" }),
      artifact({ id: "a6", path: "huge.png", size: 300 * 1024, content_type: "image/png" }),
      artifact({ id: "a7", path: "doc.pdf", size: 1000, content_type: "application/pdf" }),
    ];
    renderSection();
    await table();
    expect(screen.getByRole("button", { name: "Preview exact.txt" })).not.toBeNull();
    for (const path of ["big.txt", "huge.log", "bundle.zip", "blob.bin", "huge.png", "doc.pdf"]) {
      expect(screen.queryByRole("button", { name: `Preview ${path}` })).toBeNull();
      expect(screen.getByRole("button", { name: `Download ${path}` })).not.toBeNull();
    }
    expect(screen.getAllByRole("button", { name: /^Preview / })).toHaveLength(1);
    // nothing was fetched just to draw the table
    expect(api.made("GET", /\/artifacts\/[^/]+$/)).toHaveLength(0);
  });

  it("says why when the preview cannot be loaded, and still lets the file be downloaded", async () => {
    contents = {};
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Preview report.md" }));
    expect((await within(await previewOf("report.md")).findByRole("alert")).textContent).toBe("Could not load the preview: artifact file is missing");
    expect(screen.getByRole("button", { name: "Download report.md" })).not.toBeNull();
  });

  it("reads the file again when its newest version replaces the one on show", async () => {
    contents = { a1: { body: "first draft", type: "text/plain" } };
    const client = renderSection("running");
    fireEvent.click(await screen.findByRole("button", { name: "Preview report.md" }));
    await screen.findByText("first draft");

    // the same artifact, collected again by a later step: the id stays, the content does not
    files = [artifact({ sha256: "2".repeat(64), visit: 2, node_id: "reviewer" })];
    contents = { a1: { body: "final version", type: "text/plain" } };
    await client.invalidateQueries({ queryKey: artifactsKey(RUN) });
    expect(await screen.findByText("final version")).not.toBeNull();
    expect(screen.queryByText("first draft")).toBeNull();
    expect(screen.getByText("reviewer").parentElement!.textContent).toBe("reviewer visit 2");
    expect(api.made("GET", `/api/runs/${RUN}/artifacts/a1`)).toHaveLength(2);
  });

  it("does not read the file again when the list is refreshed and the file is the same", async () => {
    const client = renderSection("running");
    fireEvent.click(await screen.findByRole("button", { name: "Preview report.md" }));
    await screen.findByText(/All good/);
    await client.invalidateQueries({ queryKey: artifactsKey(RUN) });
    await waitFor(() => expect(api.made("GET", `/api/runs/${RUN}/artifacts`).length).toBeGreaterThan(1));
    expect(api.made("GET", `/api/runs/${RUN}/artifacts/a1`)).toHaveLength(1);
  });
});
