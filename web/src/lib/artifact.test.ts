// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PREVIEW_LIMIT, REVOKE_DELAY_MS, baseName, previewKind, saveBlob } from "./artifact";

describe("previewKind", () => {
  const kind = (content_type: string, size = 1000) => previewKind({ content_type, size });

  it.each(["text/plain", "text/markdown", "text/csv", "text/yaml", "text/x-patch", "text/html", "application/json", "image/svg+xml"])("previews %s as text", (type) => {
    expect(kind(type)).toBe("text");
  });

  it.each(["image/png", "image/jpeg", "image/gif", "image/webp"])("previews %s as an image", (type) => {
    expect(kind(type)).toBe("image");
  });

  it.each(["application/zip", "application/octet-stream", "application/pdf", "image/bmp", "image/tiff", "audio/mpeg", "application/xml", ""])("has no preview for %s", (type) => {
    expect(kind(type)).toBeUndefined();
  });

  it("ignores parameters and case in the content type", () => {
    expect(kind("text/plain; charset=utf-8")).toBe("text");
    expect(kind("Text/Markdown;charset=UTF-8")).toBe("text");
    expect(kind("IMAGE/PNG")).toBe("image");
    expect(kind("application/json; charset=utf-8")).toBe("text");
  });

  it("previews files up to 256 KiB and downloads larger ones", () => {
    expect(PREVIEW_LIMIT).toBe(262_144);
    expect(kind("text/plain", PREVIEW_LIMIT)).toBe("text");
    expect(kind("text/plain", PREVIEW_LIMIT + 1)).toBeUndefined();
    expect(kind("text/plain", 300 * 1024)).toBeUndefined();
    expect(kind("image/png", PREVIEW_LIMIT)).toBe("image");
    expect(kind("image/png", PREVIEW_LIMIT + 1)).toBeUndefined();
    expect(kind("text/plain", 0)).toBe("text");
  });
});

describe("baseName", () => {
  it("is the last segment of the path", () => {
    expect(baseName("report.md")).toBe("report.md");
    expect(baseName("reports/q3/summary.md")).toBe("summary.md");
    expect(baseName("a/b")).toBe("b");
  });

  it("falls back to the whole path when it ends in a slash", () => {
    expect(baseName("dir/")).toBe("dir/");
  });

  it("drops a right-to-left override, so a name cannot read as another type", () => {
    // shown with the override, "report\u202Egnp.exe" reads as "reportexe.png"
    expect(baseName("report\u202Egnp.exe")).toBe("reportgnp.exe");
    expect(baseName("out/report\u202Egnp.exe")).toBe("reportgnp.exe");
    expect(baseName("out\u202E/summary.md")).toBe("summary.md");
  });

  it("drops the other format characters: isolates, embeddings, marks, zero-width characters and the byte-order mark", () => {
    expect(baseName("a\u202Ab\u202Cc\u2066d\u2069e.txt")).toBe("abcde.txt");
    expect(baseName("a\u200Eb\u200Fc\u200Bd\u200De\uFEFFf.txt")).toBe("abcdef.txt");
    expect(baseName("soft\u00ADhyphen.md")).toBe("softhyphen.md");
  });

  it("drops the line and paragraph separators and the control characters", () => {
    expect(baseName("a\u2028b\u2029c.txt")).toBe("abc.txt");
    expect(baseName("a\nb\rc\td\u0000e\u007Ff\u0085g.txt")).toBe("abcdefg.txt");
  });

  it("keeps letters in any script, emoji and ordinary spaces", () => {
    expect(baseName("relatório final – 2026 ✓.md")).toBe("relatório final – 2026 ✓.md");
    expect(baseName("日本語/報告書.txt")).toBe("報告書.txt");
  });

  it("is never empty", () => {
    expect(baseName("\u202E\u2028")).toBe("download");
    expect(baseName("dir\u202E/")).toBe("dir/");
  });
});

describe("saveBlob", () => {
  let created: Blob[];
  let revoked: string[];
  let clicked: { download: string; href: string; hidden: boolean; attached: boolean }[];

  beforeEach(() => {
    vi.useFakeTimers();
    created = [];
    revoked = [];
    clicked = [];
    URL.createObjectURL = vi.fn((blob: Blob | MediaSource) => {
      created.push(blob as Blob);
      return "blob:http://localhost/one";
    });
    URL.revokeObjectURL = vi.fn((url: string) => {
      revoked.push(url);
    });
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({ download: this.download, href: this.href, hidden: this.hidden, attached: document.body.contains(this) });
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("clicks a hidden link to the object URL, named for the file", () => {
    const blob = new Blob(["hello"], { type: "text/plain" });
    saveBlob(blob, "summary.md");
    expect(created).toEqual([blob]);
    expect(clicked).toEqual([{ download: "summary.md", href: "blob:http://localhost/one", hidden: true, attached: true }]);
    // the link does not stay in the page
    expect(document.querySelector("a[download]")).toBeNull();
  });

  it("keeps the object URL for several seconds, so a browser that starts reading late still finds the file", () => {
    expect(REVOKE_DELAY_MS).toBeGreaterThanOrEqual(5_000);
    saveBlob(new Blob(["hello"]), "summary.md");
    expect(revoked).toEqual([]);
    vi.advanceTimersByTime(0);
    expect(revoked).toEqual([]);
    vi.advanceTimersByTime(1_000);
    expect(revoked).toEqual([]);
    vi.advanceTimersByTime(REVOKE_DELAY_MS - 1_001);
    expect(revoked).toEqual([]);
  });

  it("lets the object URL go once the delay has passed, once", () => {
    saveBlob(new Blob(["hello"]), "summary.md");
    vi.advanceTimersByTime(REVOKE_DELAY_MS);
    expect(revoked).toEqual(["blob:http://localhost/one"]);
    vi.advanceTimersByTime(REVOKE_DELAY_MS * 3);
    expect(revoked).toEqual(["blob:http://localhost/one"]);
  });
});
