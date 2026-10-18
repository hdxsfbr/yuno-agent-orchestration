// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PREVIEW_LIMIT, baseName, previewKind, saveBlob } from "./artifact";

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
});

describe("saveBlob", () => {
  let created: Blob[];
  let revoked: string[];
  let clicked: { download: string; href: string; hidden: boolean; attached: boolean }[];

  beforeEach(() => {
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
    vi.restoreAllMocks();
  });

  it("clicks a hidden link to the object URL, named for the file, and then lets the URL go", async () => {
    const blob = new Blob(["hello"], { type: "text/plain" });
    saveBlob(blob, "summary.md");
    expect(created).toEqual([blob]);
    expect(clicked).toEqual([{ download: "summary.md", href: "blob:http://localhost/one", hidden: true, attached: true }]);
    // the link does not stay in the page
    expect(document.querySelector("a[download]")).toBeNull();
    await vi.waitFor(() => expect(revoked).toEqual(["blob:http://localhost/one"]));
  });
});
