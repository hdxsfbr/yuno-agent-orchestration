import type { Artifact } from "./api";

// What the run page does with a file a run delivered (DESIGN 5.25 rule 7): which files it can
// show in place, and how it hands the others to the browser.

/** Files up to this size (256 KiB) can be previewed; larger ones are downloaded. */
export const PREVIEW_LIMIT = 256 * 1024;

export type PreviewKind = "text" | "image";

const IMAGES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);

/** The media type without its parameters, lower-cased: "Text/Plain; charset=utf-8" is "text/plain". */
const mediaType = (contentType: string) => contentType.split(";")[0].trim().toLowerCase();

/**
 * How a file can be previewed, if it can: `text` for text types, JSON, patches and SVG (SVG is shown
 * as its source, never drawn), `image` for PNG, JPEG, GIF and WebP; nothing for anything larger than
 * the limit and for every other type.
 */
export function previewKind(a: Pick<Artifact, "content_type" | "size">): PreviewKind | undefined {
  if (a.size > PREVIEW_LIMIT) return undefined;
  const type = mediaType(a.content_type);
  if (type.startsWith("text/") || type === "application/json" || type === "image/svg+xml") return "text";
  if (IMAGES.has(type)) return "image";
  return undefined;
}

/**
 * What must not reach a saved file name: control characters, Unicode format characters (the
 * right-to-left override U+202E turns `report\u202Egnp.exe` into a name that reads as `reportexe.png`,
 * and zero-width characters hide parts of a name) and the line and paragraph separators U+2028 and U+2029.
 */
const UNSAFE_IN_NAME = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

/** The last segment of a path without the characters of {@link UNSAFE_IN_NAME}: "reports/q3/summary.md" is "summary.md". */
export function baseName(path: string): string {
  const clean = path.replace(UNSAFE_IN_NAME, "");
  const name = clean.slice(clean.lastIndexOf("/") + 1);
  return name || clean || "download";
}

/** How long a file's object URL lives after the click: long enough for any browser to have started reading it. */
export const REVOKE_DELAY_MS = 10_000;

/**
 * Hands a blob to the browser as a file named `filename`: a link carries no token, so the file
 * is fetched with it and saved through a short-lived object URL on a hidden link.
 */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.hidden = true;
  document.body.appendChild(link);
  link.click();
  link.remove();
  // Not at once: a browser may start reading the file some time after the click, and a revoked URL gives it nothing.
  setTimeout(() => URL.revokeObjectURL(url), REVOKE_DELAY_MS);
}
