#!/usr/bin/env node
// Records the demo scenes of docs/demo-script.md from the running stack, one WebM per scene.
//
//   BASE_URL   where the UI is served (default http://web:80, from inside the compose network)
//   API_TOKEN  bearer token for the api; attached to its requests by the browser, never shown
//   SCENES     e.g. 04,06 records only those (default: all)
//   LIVE       1 lets scenes change state and spend model budget; otherwise they stop before that
//   OUT_DIR    where videos, stills, cuts.json and state.json are written (default scripts/demo/out)
//
// Runs inside the Playwright container (see the `demo` service in compose.yaml); entrypoint.sh
// installs playwright-core there first. Slow waits are recorded in cuts.json, never skipped.
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";
import { HERE, VIEWPORT, POINTER_SCRIPT, MASK_SCRIPT, SceneRecording, bind, cfg, installAuth, log, sleep, withTimeout, writeCuts } from "./helpers.mjs";
import { SCENES } from "./scenes.mjs";

const require = createRequire(path.join(process.env.PW_DIR || path.join(HERE, ".pw"), "/"));
const { chromium } = require("playwright-core");

/** Scenes that open the app (the others render files). */
const NEEDS_APP = new Set(["02", "03", "04", "05", "07", "08"]);
/** The seeded agents the scenes name. */
const DEMO_AGENTS = ["Coder", "Reviewer", "Deployer", "Researcher", "Critic", "Writer"];

function selectScenes() {
  const known = Object.keys(SCENES);
  if (!cfg.scenes.trim()) return known;
  const ids = [...new Set(cfg.scenes.split(/[\s,]+/).filter(Boolean).map((s) => s.padStart(2, "0")))].sort();
  const unknown = ids.filter((i) => !SCENES[i]);
  if (unknown.length) throw new Error(`unknown scene(s) ${unknown.join(", ")}; known: ${known.join(", ")}`);
  return ids;
}

/** Pre-flight: the stack answers, the token works, and the data the scenes assume is there. */
async function preflight() {
  const auth = cfg.token ? { Authorization: `Bearer ${cfg.token}` } : {};
  const deadline = Date.now() + 90_000;
  for (;;) {
    try {
      if ((await fetch(`${cfg.baseUrl}/readyz`, { signal: AbortSignal.timeout(5000) })).ok) break;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) throw new Error(`${cfg.baseUrl}/readyz is not answering; is the stack up (make ps)?`);
    await sleep(2000);
  }
  const agents = await fetch(`${cfg.baseUrl}/api/agents`, { headers: auth, signal: AbortSignal.timeout(10_000) });
  if (agents.status === 401 || agents.status === 403) throw new Error("the api rejected the request: API_TOKEN is missing or wrong in .env");
  if (!agents.ok) throw new Error(`GET /api/agents returned ${agents.status}`);
  const names = new Set((await agents.json()).map((a) => a.name));
  const missing = DEMO_AGENTS.filter((n) => !names.has(n));
  const status = await fetch(`${cfg.baseUrl}/api/status`, { headers: auth }).then((r) => (r.ok ? r.json() : null)).catch(() => null);
  log("pre", `stack ready at ${cfg.baseUrl}${status?.version ? `, build ${status.version}` : ""}; agents: ${[...names].join(", ") || "none"}`);
  if (missing.length) log("pre", `WARNING: seeded agents missing: ${missing.join(", ")} (make seed)`);
  const runs = await fetch(`${cfg.baseUrl}/api/runs?limit=1`, { headers: auth }).then((r) => (r.ok ? r.json() : [])).catch(() => []);
  if (runs?.length) log("pre", "note: the Runs list is not empty; scenes that show it will show those runs");
}

async function recordScene(browser, id, def, tools) {
  const rawDir = path.join(cfg.outDir, ".raw", id);
  fs.rmSync(rawDir, { recursive: true, force: true });
  fs.mkdirSync(rawDir, { recursive: true });
  const context = await browser.newContext({
    viewport: VIEWPORT,
    screen: VIEWPORT,
    deviceScaleFactor: 1,
    reducedMotion: "no-preference",
    colorScheme: "light",
    locale: "en-US",
    timezoneId: cfg.timeZone,
    recordVideo: { dir: rawDir, size: VIEWPORT },
  });
  await installAuth(context);
  context.setDefaultTimeout(20_000); // a missing element fails the step quickly; long waits pass their own timeout
  if (cfg.cursor) await context.addInitScript(POINTER_SCRIPT);
  if (cfg.maskIds) await context.addInitScript(MASK_SCRIPT);
  const page = await context.newPage();
  const rec = new SceneRecording(id); // footage time zero: the page exists, the video is rolling
  bind(page, rec);

  let error;
  try {
    await withTimeout(def.run(page, rec, tools), def.timeout, `scene ${id}`);
  } catch (err) {
    error = err;
    log(id, `FAILED: ${String(err.message).split("\n")[0]}`);
    await page.screenshot({ path: path.join(cfg.outDir, `${id}-failure.png`) }).catch(() => {});
  }
  const video = page.video();
  await context.close();
  // A failed take is kept next to, not over, the last good one, so a surgical re-record never loses footage.
  await video.saveAs(path.join(cfg.outDir, error ? `${id}.failed.webm` : `${id}.webm`));
  fs.rmSync(rawDir, { recursive: true, force: true });
  return { rec, error };
}

async function main() {
  // Stills go last: the thumbnail uses the screenshot scene 2 takes of the app in this same run.
  const ids = selectScenes().sort((a, b) => (SCENES[a].kind === "still") - (SCENES[b].kind === "still") || a.localeCompare(b));
  fs.mkdirSync(cfg.outDir, { recursive: true });
  console.log(`scenes: ${ids.join(", ")}  |  ${cfg.live ? "LIVE=1: scenes change state and spend model budget" : "dry run: stops before every state-changing click"}`);
  if (ids.some((i) => NEEDS_APP.has(i))) await preflight();

  const browser = await chromium.launch({ headless: true });
  const tools = {
    // A page with no recording, for housekeeping that must not be on screen.
    plainPage: async () => {
      const context = await browser.newContext({ viewport: VIEWPORT, locale: "en-US", timezoneId: cfg.timeZone });
      await installAuth(context);
      return { page: await context.newPage(), close: () => context.close() };
    },
  };

  const results = [];
  const recorded = [];
  try {
    for (const id of ids) {
      const def = SCENES[id];
      log(id, `${def.title}`);
      if (def.kind === "still") {
        try {
          await def.run(browser);
          results.push({ id, ok: true, note: "still" });
        } catch (err) {
          log(id, `FAILED: ${err.message}`);
          results.push({ id, ok: false, note: err.message });
        }
        continue;
      }
      const { rec, error } = await recordScene(browser, id, def, tools);
      if (!error) recorded.push(rec);
      const cut = rec.cuts.filter((c) => c.keep_head_ms !== 0).reduce((s, c) => s + (c.end_ms - c.start_ms), 0);
      results.push({ id, ok: !error, note: error ? error.message.split("\n")[0] : `${rec.cuts.length} cut(s), ${(cut / 1000).toFixed(0)} s of waits`, warnings: rec.warnings });
      if (!error) writeCuts([rec]); // keep cuts.json current even if a later scene fails
    }
  } finally {
    await browser.close();
  }
  fs.rmSync(path.join(cfg.outDir, ".raw"), { recursive: true, force: true });
  if (recorded.length === 0 && !fs.existsSync(path.join(cfg.outDir, "cuts.json"))) writeCuts([]);

  console.log("\nsummary");
  for (const r of results) {
    console.log(`  ${r.id}  ${r.ok ? "ok    " : "FAILED"}  ${r.note}`);
    for (const w of r.warnings ?? []) console.log(`        skipped: ${w}`);
  }
  if (results.some((r) => !r.ok)) process.exitCode = 1;
}

main().catch((err) => {
  console.error(`demo recorder: ${err.message}`);
  process.exit(1);
});
