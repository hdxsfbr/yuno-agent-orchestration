// The scenes of docs/demo-script.md that are recorded in a browser (2 to 8). Each is a function of a
// page that already has a recording bound to it. Scenes that spend money or change state stop before
// the state-changing click unless LIVE=1.
//
// Every wait for the model, the gateway or a human goes through waitCut(), which records the footage
// interval so post-processing can strip it. The page is never faked: what is on screen is the app.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import {
  HERE, cfg, apiGet, clickOn, heading, log, open, pause, pointAt, readState, recordingOf, runStatus,
  scrollBy, scrollTo, scrollTop, soft, trimLoad, typeInto, waitCut, writeState,
} from "./helpers.mjs";

const rx = (s) => new RegExp(s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i");
const nav = (page, name) => page.locator("aside nav").getByRole("link", { name, exact: false }).filter({ hasText: new RegExp(`^${name}`) }).first();
const runHeader = (page) => page.locator('section[aria-label="Run summary"]');
const nodeCard = (page, id) => page.locator(`section[aria-label^="Node ${id}"]`).first();
const pendingNote = (page) => page.getByText(/This run is paused until/);
/** A run-page section title with its count beside it ("Logs 12"), so the name is matched by prefix. */
const countHeading = (page, title) => page.getByRole("heading", { level: 2, name: new RegExp(`^${title}`) });

/** The run id in the current /runs/:id address. */
const runIdFromUrl = (page) => new URL(page.url()).pathname.split("/").filter(Boolean).pop();

// ---- scene 02: configuring an agent -----------------------------------------------------------

const FORM_SECTIONS = ["General", "Model", "Tools", "Limits", "Guardrails", "Memory", "Exec policy", "Skills and channels"];

/** Scrolls through the sections of the open agent form, one heading at a time. */
async function tourAgentForm(page, { dwell = 2600, only } = {}) {
  for (const name of only ?? FORM_SECTIONS) {
    const h = heading(page, name, 2);
    await scrollTo(page, h, { dwell });
    if (name === "Tools") await soft(page, "point at a tool policy", () => pointAt(page, page.locator('select[aria-label^="Policy for"]').first(), 1500));
    // A long section is read in steps rather than jumped over.
    const height = await h.evaluate((el) => el.parentElement.getBoundingClientRect().height);
    for (let left = height - 760, i = 0; left > 0 && i < 2; left -= 600, i++) await scrollBy(page, 600, 2000);
  }
}

export async function scene02(page) {
  await open(page, "/agents", heading(page, "Agents"));
  const coder = page.getByRole("link", { name: "Coder", exact: true }).first();
  await coder.waitFor();
  trimLoad(page);
  await pause(page, 1500);

  // A real screenshot of the list, kept for the thumbnail.
  fs.mkdirSync(path.join(cfg.outDir, "shots"), { recursive: true });
  await page.screenshot({ path: path.join(cfg.outDir, "shots", "agents.png") });
  await pause(page, 2500);

  await clickOn(page, coder);
  await heading(page, "Coder").waitFor();
  await page.getByText("OpenClaw agent id").waitFor(); // the runtime card has loaded
  await pause(page, 2500);

  await tourAgentForm(page);

  // The runtime card is what the narration ends on: what the gateway holds for this agent.
  await scrollTop(page, 1800);
  await soft(page, "point at the runtime files", () => pointAt(page, page.getByText("Files", { exact: true }).first(), 3500));
  await pause(page, 2000);
}

// ---- scene 03, first part: the project the pipeline works on ---------------------------------------

/** Whether the workflow scene 3 runs works on a project (its definition's `project` flag, DESIGN 5.17). */
async function pipelineNeedsProject(page) {
  const named = (list, name) => (list ?? []).find((w) => w.name === name);
  // A new copy (LIVE=1) is made from the template; otherwise the existing copy decides, or the template when there is none.
  const copy = cfg.live && process.env.NEW_COPY !== "0" ? undefined : named(await apiGet(page, "/api/workflows"), cfg.pipelineWorkflow);
  const workflow = copy ?? named(await apiGet(page, "/api/workflow-templates"), cfg.pipelineTemplate);
  return !!workflow?.definition?.project;
}

/** A project's row on the Projects page, by name. */
const projectRow = (page, name) => page.locator("tbody tr").filter({ has: page.getByRole("link", { name, exact: true }) }).first();

/**
 * The Projects page: the repository the pipeline works on, registered with a token that can push. LIVE=1
 * creates the project, or shows the one that already has its name (a previous take, or the api's
 * bootstrap), and runs Check; a dry run fills the form and cancels. Returns the name of a project the run
 * can pick, or "" when none exists. The token only goes into the form's password field: the footage
 * shows dots, and nothing here prints it.
 */
async function setupProject(page) {
  const rec = recordingOf(page);
  const name = cfg.projectName;
  const existing = name ? projectRow(page, name) : null;
  const exists = !!existing && (await existing.count()) > 0;

  if (exists && cfg.live) {
    log(rec.scene, `project ${name} exists: reusing it (delete it first to record its creation)`);
    await pointAt(page, existing.getByRole("link", { name, exact: true }), 3000);
    await checkProject(page, name);
    return name;
  }
  if (cfg.live && (!cfg.projectRepoUrl || !cfg.projectToken)) {
    throw new Error("the workflow works on a project: set PROJECT_REPO_URL and PROJECT_GIT_TOKEN in .env, or DEMO_PROJECT_NAME to an existing project's name");
  }
  if (!cfg.projectRepoUrl) {
    rec.warnings.push("PROJECT_REPO_URL is not set, so the New project form is not filled");
    return exists ? name : "";
  }
  if (!name) {
    // The name comes from the repository path; a URL without one needs DEMO_PROJECT_NAME.
    if (cfg.live) throw new Error("PROJECT_REPO_URL has no repository path to name the project after: set DEMO_PROJECT_NAME");
    rec.warnings.push("PROJECT_REPO_URL has no repository path, so the project's name is left empty");
  }

  await clickOn(page, page.getByRole("button", { name: "New project", exact: true }));
  const dialog = page.getByRole("dialog", { name: "New project" });
  await dialog.waitFor();
  await pause(page, 1200);
  await typeInto(page, dialog.getByLabel("Name"), name);
  await typeInto(page, dialog.getByLabel("Repository URL"), cfg.projectRepoUrl);
  if (cfg.projectBranch) await typeInto(page, dialog.getByLabel("Default branch"), cfg.projectBranch);
  if (cfg.projectToken) await typeInto(page, dialog.getByLabel("Access token"), cfg.projectToken, { delay: 10 });
  else rec.warnings.push("PROJECT_GIT_TOKEN is not set, so the token field is left empty");
  if (cfg.projectDescription) await typeInto(page, dialog.getByLabel("Description"), cfg.projectDescription);
  await pause(page, 1500);

  if (!cfg.live) {
    // Dry run: everything up to, not including, the click that creates the project.
    await clickOn(page, dialog.getByRole("button", { name: "Cancel", exact: true }));
    return exists ? name : "";
  }
  await clickOn(page, dialog.getByRole("button", { name: "Create project", exact: true }));
  await dialog.waitFor({ state: "hidden", timeout: 30_000 });
  await projectRow(page, name).waitFor({ timeout: 30_000 });
  await pause(page, 2500);
  await checkProject(page, name);
  return name;
}

/** Check on the project's row: the provider is asked whether the token is accepted and can push, without a clone. */
async function checkProject(page, name) {
  await soft(page, "project check", async () => {
    await clickOn(page, projectRow(page, name).getByRole("button", { name: `Check ${name}`, exact: true }));
    // The answer is a status line under the row; the api bounds the check to ten seconds.
    const result = page.locator("tbody").getByRole("status").first();
    await result.waitFor({ timeout: 30_000 });
    log(recordingOf(page).scene, `project check: ${((await result.textContent()) ?? "").trim()}`);
    await pause(page, 3500);
  });
}

/**
 * Picks the project in the Run dialog of a workflow that asks for one: `wanted` (the project scene 3 set
 * up), else the first project offered. Returns the name picked, or "" when the dialog does not ask or no
 * project exists.
 */
async function pickProject(page, dialog, wanted) {
  const rec = recordingOf(page);
  const select = dialog.getByRole("combobox", { name: /^Project/ });
  const none = dialog.getByText("works on a project repository");
  // The list of projects may still be loading when the dialog opens.
  await select.or(none).first().waitFor({ timeout: 10_000 }).catch(() => {});
  if (!(await select.count())) {
    if (await none.count()) rec.warnings.push("no project exists, so the run form cannot name one: create one on the Projects page first");
    return "";
  }
  const offered = (await select.locator("option").evaluateAll((els) => els.map((o) => ({ value: o.value, label: (o.textContent ?? "").trim() })))).filter((o) => o.value !== "");
  const choice = offered.find((o) => o.label === wanted) ?? offered[0];
  if (!choice) return "";
  if (wanted && choice.label !== wanted) rec.warnings.push(`project ${wanted} is not offered: picking ${choice.label}`);
  await pointAt(page, select, 800);
  await select.selectOption({ value: choice.value });
  await pause(page, 1500);
  return choice.label;
}

// ---- scene 03: a workflow from a template -----------------------------------------------------

/**
 * Opens an existing editable copy of the workflow (or the shipped template when only that exists).
 * Returns "copy", "template" or "none"; a dry run cannot make a copy, so "none" ends the scene early.
 */
async function openExistingWorkflow(page, name) {
  const rowLink = (row) => row.getByRole("link", { name, exact: true });
  const copy = page.locator("tr").filter({ has: page.getByRole("button", { name: rx(`Run ${name}`) }) }).first();
  if (await copy.count()) {
    await clickOn(page, rowLink(copy));
    return "copy";
  }
  const template = page.locator("tr").filter({ has: page.getByRole("button", { name: rx(`Create a copy of ${name}`) }) }).first();
  if (await template.count()) {
    await clickOn(page, rowLink(template));
    recordingOf(page).warnings.push(`no editable copy of ${name}: showing the shipped template, so no run is started`);
    return "template";
  }
  recordingOf(page).warnings.push(`no copy of ${name} exists, so the builder is not shown: record with LIVE=1 or make a copy first`);
  return "none";
}

export async function scene03(page) {
  let name = cfg.pipelineWorkflow; // the editable copy the builder shows
  const template = cfg.pipelineTemplate;
  await open(page, "/workflows", heading(page, "Workflows"));
  const fromTemplate = page.getByRole("button", { name: "From template", exact: true });
  await fromTemplate.waitFor();
  // A workflow that works on a project (the dev pipeline does) names one when its run starts, so the
  // scene registers the project first; another workflow stays on the Workflows page.
  let project = "";
  const needsProject = await pipelineNeedsProject(page);
  if (needsProject) {
    await clickOn(page, nav(page, "Projects"), { before: 0, after: 0 });
    await heading(page, "Projects").waitFor();
    await page.getByRole("button", { name: "New project", exact: true }).waitFor();
    trimLoad(page);
    await pause(page, 2000);
    project = await setupProject(page);
    await clickOn(page, nav(page, "Workflows"));
    await heading(page, "Workflows").waitFor();
    await fromTemplate.waitFor();
  } else {
    trimLoad(page);
  }
  await pause(page, 1500);

  // The template drawer. With LIVE=1 a new editable copy is made from it (NEW_COPY=0 reuses one).
  await clickOn(page, fromTemplate);
  const drawer = page.getByRole("dialog", { name: "Start from a template" });
  await drawer.waitFor();
  const card = drawer.locator("li").filter({ has: heading(page, template, 3) }).first();
  await pointAt(page, card, 3000);

  let runnable = true;
  if (cfg.live && process.env.NEW_COPY !== "0") {
    await clickOn(page, card.getByRole("button", { name: `Use ${template}` }));
    name = template; // a new copy carries the template's name
    await page.waitForURL(/\/workflows\/[^/]+$/);
  } else {
    await clickOn(page, drawer.getByRole("button", { name: "Close", exact: true }));
    const opened = await openExistingWorkflow(page, name);
    if (opened === "none") return;
    runnable = opened === "copy";
  }

  // The builder: the graph first, then the pieces the narration names.
  await heading(page, name).waitFor();
  const canvas = page.getByRole("group", { name: "Workflow canvas" });
  await canvas.waitFor();
  await pause(page, 3500);

  await soft(page, "reviewer node panel", async () => {
    await clickOn(page, page.getByRole("button", { name: "Node reviewer", exact: true }));
    await page.getByRole("region", { name: "Node settings" }).waitFor({ timeout: 5000 });
    await pause(page, 2500);
    await scrollTo(page, page.getByRole("region", { name: "Node settings" }).getByText("Output schema").first(), { offset: 140, dwell: 2500 });
  });
  await soft(page, "changes_requested edge", async () => {
    await scrollTo(page, canvas, { offset: 60, dwell: 800 });
    await clickOn(page, page.getByRole("button", { name: /^Edge reviewer to coder/ }));
    await page.getByRole("region", { name: "Edge settings" }).waitFor({ timeout: 5000 });
    await pause(page, 3200);
  });
  await soft(page, "approved edge", async () => {
    await clickOn(page, page.getByRole("button", { name: /^Edge reviewer to gate/ }));
    await pause(page, 2200);
  });
  await soft(page, "limits panel", () => scrollTo(page, heading(page, "Limits", 2), { dwell: 3200 }));

  await scrollTop(page, 1200);
  await soft(page, "validate", async () => {
    await clickOn(page, page.getByRole("button", { name: "Validate", exact: true }));
    await page.getByRole("status").filter({ hasText: /valid/i }).waitFor({ timeout: 20_000 });
    await pause(page, 1800);
  });

  if (!runnable) return;
  await clickOn(page, page.getByRole("button", { name: "Run", exact: true }));
  const dialog = page.getByRole("dialog", { name: rx(`Run ${name}`) });
  await dialog.waitFor();
  // The project the run works on comes first, when the workflow asks for one.
  const picked = await pickProject(page, dialog, project);
  // The dev pipeline's input is "task"; another workflow (a dry run against one) may name its field differently.
  const taskField = (await dialog.getByLabel("task").count()) ? dialog.getByLabel("task") : dialog.locator("textarea, input:not([type=checkbox])").first();
  await typeInto(page, taskField, cfg.pipelineTask);
  await pause(page, 1500);
  if (!cfg.live) {
    // Dry run: everything up to, not including, the click that costs money.
    await clickOn(page, dialog.getByRole("button", { name: "Cancel", exact: true }));
    return;
  }
  if (needsProject && !picked) throw new Error("the run form offers no project to run on");
  await clickOn(page, dialog.getByRole("button", { name: "Start run", exact: true }));
  await page.waitForURL(/\/runs\/[^/]+$/);
  writeState({ run_id: runIdFromUrl(page), recorded_at: new Date().toISOString() });
  await runHeader(page).waitFor();
  await pause(page, 3000);
}

// ---- scene 04: a successful run, live -------------------------------------------------------------

/** The run the scene follows: RUN_ID, the one scene 3 started, or (dry run) the latest finished one. */
async function chooseRun(page) {
  const wanted = cfg.runId || readState().run_id;
  if (wanted) return wanted;
  if (cfg.live) throw new Error("no run to follow: record scene 03 with LIVE=1 first, or pass RUN_ID");
  const runs = await apiGet(page, "/api/runs?limit=100");
  const done = (runs ?? []).find((r) => r.status === "succeeded" && (r.agent_names?.length ?? 0) > 1) ?? (runs ?? []).find((r) => r.status === "succeeded");
  if (!done) throw new Error("no run exists to show; start one or pass RUN_ID");
  log("04", "dry run: showing an existing finished run, nothing is started or decided");
  return done.id;
}

/** Decide the approval card that belongs to this run (in the inbox or on the run page). */
async function decide(page, card, button, reason) {
  await typeInto(page, card.getByLabel("Reason"), reason);
  await clickOn(page, card.getByRole("button", { name: button, exact: true }));
}

export async function scene04(page) {
  await open(page, "/runs", heading(page, "Runs"));
  trimLoad(page);
  const runId = await chooseRun(page);
  const runUrl = `/runs/${runId}`;
  await page.goto(cfg.baseUrl + runUrl, { waitUntil: "domcontentloaded" });
  await runHeader(page).waitFor({ timeout: 30_000 });
  await pause(page, 2000);

  if (cfg.live) {
    const patience = { timeout: cfg.runTimeoutMs };
    // Coder works: model calls and sandboxed exec calls appear in its node.
    await waitCut(page, "coder starts and calls its tools", () => nodeCard(page, "coder").getByText("Tool call", { exact: true }).first().waitFor(patience));
    await soft(page, "look at the coder's calls", async () => {
      await scrollTo(page, nodeCard(page, "coder"), { dwell: 4500 });
    });
    // The hand-over to Reviewer, then the human gate.
    await waitCut(page, "coder finishes, reviewer reviews", () => nodeCard(page, "reviewer").waitFor(patience));
    await soft(page, "agent message to the reviewer", () => scrollTo(page, countHeading(page, "Agent messages"), { dwell: 4500 }));
    await waitCut(page, "reviewer approves, run reaches the human gate", () => pendingNote(page).waitFor(patience));
    await scrollTop(page, 2500);

    // The inbox: the badge, the card, the decision.
    await soft(page, "inbox badge", () => pointAt(page, nav(page, "Approvals"), 2500));
    await clickOn(page, nav(page, "Approvals"));
    await heading(page, "Approvals").waitFor();
    const gate = page.locator("article").filter({ has: page.locator(`a[href="/runs/${runId}"]`) }).first();
    await gate.waitFor({ timeout: 30_000 });
    await pause(page, 3000);
    await decide(page, gate, "Approve", "looks good");
    await pause(page, 2500);
    await page.goBack();
    await runHeader(page).waitFor();

    // Deployer asks to run a side-effecting tool: the call itself blocks. On a project it opens a merge
    // request; a run without one publishes a release.
    const pendingDeploy = page.locator("article").filter({ hasText: /open_merge_request|deploy_release/ }).filter({ has: page.getByRole("button", { name: "Approve", exact: true }) });
    await waitCut(page, "deployer calls its side-effecting tool, which waits for approval", () => pendingDeploy.first().waitFor(patience));
    const deploy = pendingDeploy.first();
    const tool = /open_merge_request/.test((await deploy.textContent()) ?? "") ? "open_merge_request" : "deploy_release";
    const opensRequest = tool === "open_merge_request";
    await scrollTo(page, deploy, { offset: 120, dwell: 4500 });
    await decide(page, deploy, "Approve", opensRequest ? "merge request looks right" : "release looks right");
    await waitCut(page, opensRequest ? "the merge request is opened and the run finishes" : "release is published and the run finishes", () =>
      runStatus(page).filter({ hasText: /succeeded/i }).waitFor(patience));
    await soft(page, "failed or cancelled run", async () => {
      if (!/succeeded/i.test((await runStatus(page).textContent()) ?? "")) throw new Error("the run did not succeed");
    });
    await pause(page, 2000);

    // The merge request's address (or the release URL) is in the tool call's output.
    await soft(page, `${tool} output`, async () => {
      const node = nodeCard(page, "deployer");
      await scrollTo(page, node, { dwell: 1500 });
      // The Output of the side-effecting call itself (not of an earlier call in the same node).
      const call = node.locator("li").filter({ hasText: new RegExp(`Tool call\\s*${tool}`) }).first();
      await clickOn(page, call.locator("summary").filter({ hasText: /^Output/ }).first());
      await pause(page, 4500);
    });
  }

  // The trace: counters and cost, the timeline, the state, the messages, the logs.
  await scrollTop(page, 1500);
  await pointAt(page, runHeader(page), 4500);
  // A run on a project: the project it worked on, and the merge request it opened, in the summary.
  await soft(page, "project and merge request", async () => {
    const header = runHeader(page);
    const project = header.getByText("Project", { exact: true });
    if (!(await project.count())) return;
    await pointAt(page, project, 2500);
    const request = header.getByRole("link", { name: /^(Merge|Pull) request/ });
    if (await request.count()) await pointAt(page, request, 3000);
  });
  await soft(page, "timeline", () => scrollTo(page, heading(page, "Timeline", 2), { dwell: 4500 }));
  await soft(page, "first node card", () => scrollTo(page, page.locator('section[aria-label^="Node "]').first(), { offset: 80, dwell: 4500 }));
  await soft(page, "state panel", () => scrollTo(page, heading(page, "State", 2), { dwell: 4500 }));
  await soft(page, "agent messages", () => scrollTo(page, countHeading(page, "Agent messages"), { dwell: 4500 }));
  await soft(page, "logs", () => scrollTo(page, countHeading(page, "Logs"), { dwell: 4500 }));
}

// ---- scene 05: a handled failure -----------------------------------------------------------------

/** Sets the Max steps limit on Coder's form and saves it. Used to set 2 and to put the old value back. */
async function setCoderMaxSteps(page, value, { pace = true } = {}) {
  await open(page, "/agents", heading(page, "Agents"));
  const link = page.getByRole("link", { name: "Coder", exact: true }).first();
  if (pace) await clickOn(page, link);
  else await link.click();
  await heading(page, "Coder").waitFor();
  const field = page.getByLabel("Max steps");
  await scrollTo(page, heading(page, "Limits", 2), { dwell: pace ? 1800 : 0 });
  const previous = await field.inputValue();
  if (pace) await clickOn(page, field, { after: 100 });
  await field.fill(value);
  if (pace) await pause(page, 1500);
  await clickOn(page, page.getByRole("button", { name: "Save changes", exact: true }), { before: pace ? 350 : 0, after: pace ? 800 : 0 });
  await page.getByRole("status").filter({ hasText: /saved/i }).waitFor({ timeout: 60_000 });
  return previous;
}

export async function scene05(page, rec, tools) {
  await open(page, "/agents", heading(page, "Agents"));
  trimLoad(page);
  await pause(page, 1500);

  if (!cfg.live) {
    // Dry run: show the limit field and the failed runs already in the list, change nothing.
    await clickOn(page, page.getByRole("link", { name: "Coder", exact: true }).first());
    await heading(page, "Coder").waitFor();
    await scrollTo(page, heading(page, "Limits", 2), { dwell: 3500 });
    await pointAt(page, page.getByLabel("Max steps"), 2000);
    await clickOn(page, nav(page, "Runs"));
    await heading(page, "Runs").waitFor();
    await soft(page, "failed runs", async () => {
      await page.getByLabel("Status").selectOption("failed");
      await pause(page, 3500);
    });
    await clickOn(page, nav(page, "Approvals"));
    await heading(page, "Approvals").waitFor();
    await soft(page, "rejected approvals", async () => {
      await page.getByLabel("Show").selectOption("rejected");
      await pause(page, 3500);
    });
    return;
  }

  // Part 1: the step limit. Coder's Max steps is set to 2 for the run and put back afterwards.
  let previous = "";
  let changed = false;
  try {
    await clickOn(page, page.getByRole("link", { name: "Coder", exact: true }).first());
    await heading(page, "Coder").waitFor();
    await scrollTo(page, heading(page, "Limits", 2), { dwell: 1500 });
    const field = page.getByLabel("Max steps");
    previous = await field.inputValue();
    await clickOn(page, field, { after: 100 });
    await field.fill("2");
    await pause(page, 1500);
    changed = true; // from here on the saved limit may differ from the original
    await clickOn(page, page.getByRole("button", { name: "Save changes", exact: true }));
    await page.getByRole("status").filter({ hasText: /saved/i }).waitFor({ timeout: 60_000 });
    await scrollTop(page, 800);
    await clickOn(page, page.getByRole("button", { name: "Run", exact: true }));
    const dialog = page.getByRole("dialog", { name: "Run Coder" });
    await dialog.waitFor();
    await typeInto(page, dialog.getByLabel("Task"), cfg.stepLimitTask);
    await clickOn(page, dialog.getByRole("button", { name: "Start run", exact: true }));
    await page.waitForURL(/\/runs\/[^/]+$/);
    await runHeader(page).waitFor();
    await pause(page, 1500);

    await waitCut(page, "coder hits the step limit", () =>
      runHeader(page).getByRole("alert").filter({ hasText: /step limit 2 exceeded/i }).waitFor({ timeout: cfg.runTimeoutMs }));
    await pause(page, 3500);
    await soft(page, "abort after the second model call", () => scrollTo(page, heading(page, "Timeline", 2), { dwell: 5000 }));
    await clickOn(page, nav(page, "Runs"));
    await heading(page, "Runs").waitFor();
    await pause(page, 4000);
  } finally {
    // Not part of the footage: put Coder's limit back the way it was.
    if (changed) {
      const plain = await tools.plainPage();
      try {
        await setCoderMaxSteps(plain.page, previous, { pace: false });
        log("05", `Coder's Max steps restored to ${previous === "" ? "its default (empty)" : previous}`);
      } finally {
        await plain.close();
      }
    }
  }

  // Part 2: a rejected approval is a result the agent can act on. Deployer asks; the answer is no.
  await clickOn(page, nav(page, "Agents"));
  await heading(page, "Agents").waitFor();
  const row = page.locator("tr").filter({ has: page.getByRole("link", { name: "Deployer", exact: true }) });
  await clickOn(page, row.getByRole("button", { name: "Run", exact: true }));
  const dialog = page.getByRole("dialog", { name: "Run Deployer" });
  await dialog.waitFor();
  await typeInto(page, dialog.getByLabel("Task"), cfg.rejectTask);
  await clickOn(page, dialog.getByRole("button", { name: "Start run", exact: true }));
  await page.waitForURL(/\/runs\/[^/]+$/);
  const runId = runIdFromUrl(page);
  await runHeader(page).waitFor();
  await waitCut(page, "deployer asks for approval", () => pendingNote(page).waitFor({ timeout: cfg.runTimeoutMs }));
  await clickOn(page, nav(page, "Approvals"));
  await heading(page, "Approvals").waitFor();
  const card = page.locator("article").filter({ has: page.locator(`a[href="/runs/${runId}"]`) }).first();
  await card.waitFor({ timeout: 30_000 });
  await pause(page, 2500);
  await decide(page, card, "Reject", "not approved for release");
  await pause(page, 2000);
  await page.goBack();
  await runHeader(page).waitFor();
  await waitCut(page, "deployer finishes after the rejection", () =>
    runStatus(page).filter({ hasText: /succeeded|failed|cancelled|timed out/i }).waitFor({ timeout: cfg.runTimeoutMs }));
  await pause(page, 2500);
  await soft(page, "state shows the rejection", () => scrollTo(page, heading(page, "State", 2), { dwell: 5000 }));
}

// ---- scene 06: evaluations -------------------------------------------------------------------

const firstReadable = (...files) => files.filter(Boolean).find((f) => fs.existsSync(f));

export async function scene06(page) {
  const reportFile = firstReadable(cfg.evalReport, "/reports/baseline.md", path.join(HERE, "..", "..", "evals", "reports", "baseline.md"));
  if (!reportFile) throw new Error("evals/reports/baseline.md not found (mount ./evals/reports at /reports or set EVAL_REPORT)");
  const markdown = fs.readFileSync(reportFile, "utf8");
  const consoleFile = firstReadable(cfg.evalConsole, path.join(cfg.outDir, "eval-console.log"));

  if (consoleFile) {
    // The captured console of a real `make eval`, ending in its summary line.
    await page.addInitScript((text) => (window.__CONSOLE__ = text), fs.readFileSync(consoleFile, "utf8"));
    await page.goto(pathToFileURL(path.join(HERE, "cards", "terminal.html")).href);
    await page.locator("pre").waitFor();
    trimLoad(page);
    await pause(page, 7000);
  }

  await page.addInitScript((text) => (window.__REPORT_MD__ = text), markdown);
  await page.goto(pathToFileURL(path.join(HERE, "cards", "report.html")).href);
  await heading(page, "Cases", 2).waitFor();
  if (!consoleFile) trimLoad(page);
  await pause(page, 4500);

  for (const [name, dwell] of [["Overall", 4500], ["Evaluators", 4500], ["Cases", 3500]]) {
    await scrollTo(page, heading(page, name, 2), { dwell });
  }
  // The per-case table is long: read it in steps, evaluator columns and rubric column in view.
  const tableHeight = await page.locator("h2:text-is('Cases') + table").evaluate((el) => el.getBoundingClientRect().height);
  for (let left = tableHeight - 700, i = 0; left > 0 && i < 4; left -= 560, i++) await scrollBy(page, 560, 2200);
  await soft(page, "rubric by criterion", () => scrollTo(page, heading(page, "Rubric by criterion", 2), { dwell: 4500 }));
  await scrollTop(page, 2500);
}

// ---- scene 07: a live Telegram conversation --------------------------------------------------------

const bubbles = (page) => page.locator('section[aria-label^="Conversation with"] li[data-direction]');

export async function scene07(page) {
  await open(page, "/channels", heading(page, "Channels"));
  const telegram = heading(page, "Telegram", 2);
  await telegram.waitFor();
  trimLoad(page);
  await pause(page, 2500);

  // The channel card: bot, allowlisted users, the bound agent.
  for (const label of ["Bot", "Allowed users", "Agent"]) {
    await soft(page, `telegram card: ${label}`, () => pointAt(page, page.getByText(label, { exact: true }).first(), 1800));
  }
  await soft(page, "conversations", () => scrollTo(page, heading(page, "Conversations", 2), { offset: 100, dwell: 2500 }));

  // The bound agent's conversation.
  const agentName = await soft(page, "bound agent name", async () => {
    // The card's unbind button is named after the agent: "Unbind Coder".
    const unbind = page.getByRole("button", { name: /^Unbind / });
    await unbind.waitFor({ timeout: 3000 });
    return ((await unbind.getAttribute("aria-label")) ?? "").replace(/^Unbind /, "");
  });
  const conversations = page.locator('a[href^="/channels/"]');
  const mine = agentName ? conversations.filter({ hasText: agentName }).first() : conversations.first();
  if (!(await mine.count())) {
    recordingOf(page).warnings.push("no conversation exists yet: send the bot a message first, or the transcript cannot be shown");
    return;
  }
  await clickOn(page, mine);
  await page.locator('section[aria-label^="Conversation with"]').waitFor();
  await pause(page, 3000);

  if (!cfg.live) return; // the wait for a person's message is the live part

  // A person sends a message from the phone; it must appear here, followed by the agent's reply.
  const before = await bubbles(page).count();
  log("07", "WAITING: send the bot a Telegram message from the phone now");
  await waitCut(page, "message arrives from the phone", () =>
    page.waitForFunction(
      ([sel, n]) => document.querySelectorAll(sel).length > n,
      ['section[aria-label^="Conversation with"] li[data-direction]', before],
      { timeout: cfg.phoneTimeoutMs },
    ));
  await pause(page, 2500);
  await waitCut(page, "agent replies", () =>
    page.waitForFunction(
      ([sel, n]) => Array.from(document.querySelectorAll(sel)).slice(n).some((li) => li.getAttribute("data-direction") === "out"),
      ['section[aria-label^="Conversation with"] li[data-direction]', before],
      { timeout: cfg.runTimeoutMs },
    ));
  await pause(page, 4000);
  await soft(page, "the reply's run", async () => {
    const link = bubbles(page).last().getByRole("link", { name: /^Run / });
    await clickOn(page, link);
    await runHeader(page).waitFor();
    await pause(page, 4000);
  });
}

// ---- scene 08: schedules and status ---------------------------------------------------------------------

export async function scene08(page) {
  await open(page, "/schedules", heading(page, "Schedules"));
  await page.getByRole("button", { name: "New schedule", exact: true }).waitFor();
  trimLoad(page);
  await pause(page, 2000);

  await clickOn(page, page.getByRole("button", { name: "New schedule", exact: true }));
  const dialog = page.getByRole("dialog", { name: "New schedule" });
  await dialog.waitFor();
  await pause(page, 1200);
  await dialog.getByLabel("Workflow").selectOption({ label: cfg.scheduleWorkflow });
  await pause(page, 1200);
  await dialog.getByLabel("Runs").selectOption({ label: "Once, at a set time" });
  await pause(page, 1200);
  // Two minutes from now, in the browser's own time zone (the form says which).
  const when = await page.evaluate((ms) => {
    const d = new Date(Date.now() + ms);
    const p = (n) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
  }, 120_000);
  await dialog.getByLabel("Run at").fill(when);
  await pause(page, 1500);
  const topic = dialog.getByLabel("topic");
  if (await topic.count()) await typeInto(page, topic, cfg.scheduleTopic);
  await pause(page, 1500);

  if (!cfg.live) {
    // Dry run: the form is complete; stop before the click that creates the schedule.
    await clickOn(page, dialog.getByRole("button", { name: "Cancel", exact: true }));
  } else {
    // The new schedule is the row whose enable switch was not on the page before; its label (workflow and
    // time) then identifies the row for as long as the wait lasts, however the list is re-ordered.
    const before = await page.locator('input[role="switch"]').evaluateAll((els) => els.map((e) => e.getAttribute("aria-label")));
    await clickOn(page, dialog.getByRole("button", { name: "Create schedule", exact: true }));
    await dialog.waitFor({ state: "hidden", timeout: 30_000 });
    const added = await page.waitForFunction(
      (known) => [...document.querySelectorAll('input[role="switch"]')].map((e) => e.getAttribute("aria-label")).find((l) => !known.includes(l)) || false,
      before,
      { timeout: 30_000 },
    );
    const row = page.locator("tr").filter({ has: page.getByRole("switch", { name: await added.jsonValue(), exact: true }) });
    await row.waitFor();
    await pause(page, 3000);

    // The scheduler fires it. The wait is cut; the page is reloaded now and then in case an event was missed.
    const fired = row.getByRole("link", { name: /^Run / });
    await waitCut(page, "the schedule fires", async () => {
      const deadline = Date.now() + cfg.scheduleWaitMs;
      for (;;) {
        if (await fired.count()) return;
        if (Date.now() > deadline) throw new Error("the schedule did not fire in time");
        await page.waitForTimeout(10_000);
        if (!(await fired.count())) await page.reload({ waitUntil: "domcontentloaded" });
      }
    });
    await pause(page, 2500);
    await clickOn(page, nav(page, "Runs"));
    await heading(page, "Runs").waitFor();
    await soft(page, "scheduled run in the list", () => pointAt(page, page.locator("tr").filter({ hasText: "schedule" }).first(), 4000));
  }

  await clickOn(page, nav(page, "Status"));
  await heading(page, "Status").waitFor();
  await page.getByText("Gateway", { exact: true }).waitFor({ timeout: 30_000 });
  await pause(page, 3500);
  await soft(page, "openclaw panel", () => scrollTo(page, heading(page, "OpenClaw", 2), { offset: 80, dwell: 3500 }));
  await soft(page, "scheduler panel", () => scrollTo(page, heading(page, "Scheduler", 2), { offset: 200, dwell: 4000 }));
  await scrollTop(page, 1500);
}

// ---- stills: title and closing card, and the thumbnail ------------------------------------------------

async function renderCard(browser, file, data) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, reducedMotion: "no-preference" });
  const page = await context.newPage();
  await page.addInitScript((d) => (window.__CARD__ = d), data);
  await page.goto(pathToFileURL(path.join(HERE, "cards", file)).href, { waitUntil: "load" });
  await page.evaluate(() => document.fonts.ready);
  return { page, close: () => context.close() };
}

async function still(browser, { id, card, data, seconds, out = `${id}.png` }) {
  const r = await renderCard(browser, card, data);
  try {
    await r.page.screenshot({ path: path.join(cfg.outDir, out) });
  } finally {
    await r.close();
  }
  if (seconds) fs.writeFileSync(path.join(cfg.outDir, `${id}.seconds`), `${seconds}\n`);
  log(id, `wrote ${out}${seconds ? ` (${seconds} s)` : ""}`);
}

const dataUrl = (file) => (fs.existsSync(file) ? `data:image/png;base64,${fs.readFileSync(file).toString("base64")}` : "");

export async function scene01(browser) {
  await still(browser, { id: "01", card: "title.html", data: { commit: cfg.commit, date: cfg.date }, seconds: 5 });
  // The thumbnail: the product name over a real screenshot of the app, when scene 2 has captured one.
  await still(browser, { id: "thumbnail", card: "thumbnail.html", data: { shot: dataUrl(path.join(cfg.outDir, "shots", "agents.png")) }, out: "thumbnail-src.png" });
}

export async function scene09(browser) {
  await still(browser, { id: "09", card: "closing.html", data: { repo: cfg.repo }, seconds: 10 });
}

// ---- registry -----------------------------------------------------------------------------------------------

/** kind "still" scenes get the browser and write PNGs; "video" scenes get a recorded page. */
export const SCENES = {
  "01": { kind: "still", title: "Title", run: scene01 },
  "02": { kind: "video", title: "Configuring an agent", run: scene02, timeout: 5 * 60_000 },
  "03": { kind: "video", title: "Building a workflow from a template", run: scene03, timeout: 5 * 60_000 },
  "04": { kind: "video", title: "A successful run, live", run: scene04, timeout: 40 * 60_000 },
  "05": { kind: "video", title: "A handled failure", run: scene05, timeout: 30 * 60_000 },
  "06": { kind: "video", title: "Evaluations", run: scene06, timeout: 5 * 60_000 },
  "07": { kind: "video", title: "A live Telegram conversation", run: scene07, timeout: 30 * 60_000 },
  "08": { kind: "video", title: "Schedules and status", run: scene08, timeout: 20 * 60_000 },
  "09": { kind: "still", title: "Closing", run: scene09 },
};
