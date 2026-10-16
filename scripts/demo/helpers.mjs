// Shared plumbing for the demo recorder: configuration from the environment, the cut recorder
// (waitCut), pacing, a visible pointer, and small page helpers. Nothing here knows a scene.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const HERE = path.dirname(fileURLToPath(import.meta.url));
export const VIEWPORT = { width: 1440, height: 900 };

const flag = (v) => v === "1" || v === "true" || v === "yes";
const num = (v, d) => (Number.isFinite(Number(v)) && v !== undefined && v !== "" ? Number(v) : d);

export const cfg = {
  baseUrl: (process.env.BASE_URL || "http://web:80").replace(/\/+$/, ""),
  // The token is only ever put on requests to baseUrl's origin by installAuth(); never printed.
  token: process.env.API_TOKEN || "",
  live: flag(process.env.LIVE),
  scenes: process.env.SCENES || "",
  outDir: path.resolve(process.env.OUT_DIR || path.join(HERE, "out")),
  // Multiplies every deliberate pause; a dry run can use PACE=0.3.
  pace: num(process.env.PACE, 1),
  cursor: process.env.CURSOR !== "0",
  maskIds: flag(process.env.MASK_IDS),
  timeZone: process.env.DEMO_TZ || "UTC",
  commit: process.env.COMMIT || "",
  date: process.env.DEMO_DATE || new Date().toISOString().slice(0, 10),
  repo: process.env.REPO_URL || "",
  runId: process.env.RUN_ID || "",
  // The task the dev-pipeline run is started with (scene 3) and the other prompts.
  pipelineTask: process.env.PIPELINE_TASK || "implement a function that returns the nth Fibonacci number, with tests",
  stepLimitTask:
    process.env.STEP_LIMIT_TASK ||
    "Write a Python module with three string helpers (slugify, truncate, title_case), write tests for each, run the tests, and fix anything that fails.",
  rejectTask:
    process.env.REJECT_TASK ||
    "Create a file hello.txt that says hello in runs/demo/ of your workspace, then publish it as release 0.0.1 with the deploy_release tool and report the result.",
  scheduleTopic: process.env.SCHEDULE_TOPIC || "How Python projects choose a packaging tool",
  scheduleWorkflow: process.env.SCHEDULE_WORKFLOW || "Research pipeline",
  // The template the drawer offers, and the name of the editable copy the builder scene opens.
  pipelineTemplate: process.env.PIPELINE_TEMPLATE || "Dev pipeline",
  pipelineWorkflow: process.env.PIPELINE_WORKFLOW || process.env.PIPELINE_TEMPLATE || "Dev pipeline",
  // Where the long waits stop being patient. Generous: a real model is slow, and the wait is cut.
  runTimeoutMs: num(process.env.RUN_TIMEOUT_MS, 15 * 60_000),
  phoneTimeoutMs: num(process.env.PHONE_TIMEOUT_MS, 10 * 60_000),
  scheduleWaitMs: num(process.env.SCHEDULE_WAIT_MS, 4 * 60_000),
  evalReport: process.env.EVAL_REPORT || "",
  evalConsole: process.env.EVAL_CONSOLE || "",
};

export const pause = (page, ms) => page.waitForTimeout(Math.max(0, Math.round(ms * cfg.pace)));

// ---- cuts -----------------------------------------------------------------------------------

/** One recording: when its footage began, and the intervals of it that are dead air. */
export class SceneRecording {
  constructor(scene) {
    this.scene = scene;
    this.t0 = Date.now();
    this.cuts = [];
    // Steps that were skipped because the page did not look as the script expected.
    this.warnings = [];
  }
  now() {
    return Date.now() - this.t0;
  }
}

const recordings = new WeakMap();
export const bind = (page, rec) => recordings.set(page, rec);
export const recordingOf = (page) => {
  const rec = recordings.get(page);
  if (!rec) throw new Error("page has no recording bound to it");
  return rec;
};

/**
 * Runs fn (a slow wait for the backend) and records the footage interval it covered. Post
 * processing strips the interval except for the click's pending state and the moment just before
 * the result, so call it right after the action that started the wait.
 */
export async function waitCut(page, label, fn, extra = {}) {
  const rec = recordingOf(page);
  const start_ms = rec.now();
  log(rec.scene, `waiting: ${label}`);
  try {
    return await fn();
  } finally {
    const end_ms = rec.now();
    rec.cuts.push({ scene: rec.scene, label, start_ms, end_ms, ...extra });
    log(rec.scene, `done: ${label} (${((end_ms - start_ms) / 1000).toFixed(1)} s of footage)`);
  }
}

const LEAD_MS = 400;

/** Marks the blank page before the first render as dead footage: stripped entirely. */
export function trimLoad(page) {
  const rec = recordingOf(page);
  // The recorder writes a blank frame before the first paint and the clock here starts a moment after the
  // video does, so the strip runs LEAD_MS past the page being ready.
  rec.cuts.push({ scene: rec.scene, label: "page load", start_ms: 0, end_ms: rec.now() + LEAD_MS, keep_head_ms: 0, keep_tail_ms: 0 });
}

export const log = (scene, msg) => console.log(`[${scene}] ${msg}`);

// ---- cuts.json / state.json -------------------------------------------------------------------

const readJson = (file, fallback) => {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return fallback;
  }
};

/** Replaces the entries of the scenes just recorded, keeping every other scene's cuts. */
export function writeCuts(recordings_) {
  const file = path.join(cfg.outDir, "cuts.json");
  const mine = new Set(recordings_.map((r) => r.scene));
  const kept = readJson(file, []).filter((c) => !mine.has(c.scene));
  const all = [...kept, ...recordings_.flatMap((r) => r.cuts)].sort((a, b) => a.scene.localeCompare(b.scene) || a.start_ms - b.start_ms);
  // One object per line: stitch.sh reads it with jq, but a person can read it too.
  fs.writeFileSync(file, "[\n" + all.map((c) => "  " + JSON.stringify(c)).join(",\n") + "\n]\n");
}

export const readState = () => readJson(path.join(cfg.outDir, "state.json"), {});
export function writeState(patch) {
  fs.writeFileSync(path.join(cfg.outDir, "state.json"), JSON.stringify({ ...readState(), ...patch }, null, 2) + "\n");
}

// ---- authorisation --------------------------------------------------------------------------

/**
 * Adds the bearer token to the page's own API calls. It is attached at the network layer for
 * this origin only, so it is never typed, rendered, stored in the page, or sent anywhere else.
 */
export async function installAuth(context) {
  if (!cfg.token) return;
  const origin = new URL(cfg.baseUrl).origin;
  await context.route(
    (url) => url.origin === origin && url.pathname.startsWith("/api/"),
    (route) => route.continue({ headers: { ...route.request().headers(), authorization: `Bearer ${cfg.token}` } }),
  );
}

// ---- the pointer ----------------------------------------------------------------------------

// Playwright does not draw a cursor into the video; without one a viewer cannot follow a click.
export const POINTER_SCRIPT = `(() => {
  if (window.top !== window) return;
  const dot = document.createElement("div");
  dot.setAttribute("aria-hidden", "true");
  dot.style.cssText = "position:fixed;left:0;top:0;width:20px;height:20px;margin:-10px 0 0 -10px;border-radius:50%;" +
    "background:rgba(24,24,27,.55);border:2px solid #fff;box-shadow:0 1px 5px rgba(0,0,0,.4);pointer-events:none;" +
    "z-index:2147483647;display:none;transition:width .12s,height .12s,margin .12s";
  const mount = () => document.documentElement.appendChild(dot);
  if (document.documentElement) mount(); else document.addEventListener("DOMContentLoaded", mount);
  addEventListener("mousemove", (e) => { dot.style.display = "block"; dot.style.left = e.clientX + "px"; dot.style.top = e.clientY + "px"; }, true);
  addEventListener("mousedown", () => { dot.style.width = dot.style.height = "30px"; dot.style.margin = "-15px 0 0 -15px"; }, true);
  addEventListener("mouseup", () => { dot.style.width = dot.style.height = "20px"; dot.style.margin = "-10px 0 0 -10px"; }, true);
})();`;

// ---- page helpers ---------------------------------------------------------------------------

/** Glides the pointer to the element, then clicks it, so the viewer sees where the click lands. */
export async function clickOn(page, locator, { before = 350, after = 500 } = {}) {
  await locator.scrollIntoViewIfNeeded();
  const box = await locator.boundingBox();
  if (box) await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 22 });
  await pause(page, before);
  await locator.click();
  await pause(page, after);
}

/** Moves the pointer onto an element without clicking: points at it while the narration names it. */
export async function pointAt(page, locator, dwell = 1200) {
  await locator.scrollIntoViewIfNeeded();
  const box = await locator.boundingBox();
  if (box) await page.mouse.move(box.x + Math.min(box.width / 2, 240), box.y + box.height / 2, { steps: 22 });
  await pause(page, dwell);
}

/** Types like a person at a keyboard, not a paste. */
export async function typeInto(page, locator, text, { delay = 28 } = {}) {
  await clickOn(page, locator, { after: 150 });
  await locator.pressSequentially(text, { delay: Math.max(1, Math.round(delay * cfg.pace)) });
  await pause(page, 400);
}

/** Smoothly scrolls the element near the top of the view and lets the viewer read it. */
export async function scrollTo(page, locator, { offset = 24, dwell = 2200 } = {}) {
  await locator.first().evaluate((el, off) => {
    const y = el.getBoundingClientRect().top + window.scrollY - off;
    window.scrollTo({ top: Math.max(0, y), behavior: "smooth" });
  }, offset);
  await pause(page, dwell);
}

export const scrollTop = (page, dwell = 1800) => scrollBy(page, -1e6, dwell);

/** Runs a flourish (a scroll, a pointer move). If the page does not look as expected the take goes on. */
export async function soft(page, what, fn) {
  const rec = recordingOf(page);
  try {
    return await fn();
  } catch (err) {
    rec.warnings.push(what);
    log(rec.scene, `SKIPPED ${what}: ${String(err.message).split("\n")[0]}`);
  }
}

/** Eases the whole page down by a number of pixels. */
export async function scrollBy(page, dy, dwell = 1500) {
  await page.evaluate((y) => window.scrollBy({ top: y, behavior: "smooth" }), dy);
  await pause(page, dwell);
}

/** Opens a path in the app and waits until the given locator shows that the page has rendered. */
export async function open(page, p, ready) {
  await page.goto(cfg.baseUrl + p, { waitUntil: "domcontentloaded" });
  await ready.first().waitFor({ state: "visible", timeout: 30_000 });
}

/** GET an API path through the page, so it carries the same authorisation the UI does. */
export async function apiGet(page, p) {
  const res = await page.evaluate(async (u) => {
    const r = await fetch(u, { headers: { Accept: "application/json" } });
    return { status: r.status, text: await r.text() };
  }, p);
  if (res.status >= 400) throw new Error(`GET ${p} returned ${res.status}`);
  return res.text ? JSON.parse(res.text) : undefined;
}

export const heading = (page, name, level = 1) => page.getByRole("heading", { name, level, exact: true });

/** The run page's status badge text, lower case as the UI shows it ("succeeded", "waiting approval"). */
export const runStatus = (page) => page.locator('section[aria-label="Run summary"] h1 + span');

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Rejects after ms, so a stuck scene fails loudly instead of holding the whole recording. */
export function withTimeout(promise, ms, what) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${Math.round(ms / 1000)} s`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

// ---- masking ----------------------------------------------------------------------------------

// MASK_IDS=1 blurs standalone numeric ids of 7 digits or more (a Telegram user id) wherever they show.
export const MASK_SCRIPT = `(() => {
  const blur = () => {
    for (const el of document.querySelectorAll("main *")) {
      if (el.children.length === 0 && /^\\d{7,}$/.test((el.textContent || "").trim())) el.style.filter = "blur(7px)";
    }
  };
  new MutationObserver(blur).observe(document, { childList: true, subtree: true, characterData: true });
  addEventListener("DOMContentLoaded", blur);
})();`;
