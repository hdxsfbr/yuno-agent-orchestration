#!/usr/bin/env node
// Sandbox override probe (DESIGN 5.16 rule 4): verifies that a per-agent
// agents.entries.<id>.sandbox block set through config.patch picks the Docker image and
// network of that agent's next session, that a new session (scope "session") gets a
// fresh container with the current settings while the global defaults stay as they
// are, and that deleting the block (null, listed in replacePaths) restores the defaults.
// It also prints the reload kind of the paths the platform patches and whether the
// gateway offers sandbox.recreate.
//
// Runs inside the gateway's network namespace (compose service `probe`), with a mock
// model server on 127.0.0.1:9999 like openclaw-probe.mjs. Cleans up what it created.
// Exit 0 when every check passed.
//
// Usage: node scripts/openclaw-probe-sandbox.mjs [--keep]
//   env: OPENCLAW_GATEWAY_TOKEN (required)
//        PROBE_IMAGE_DEFAULT  the baseline sandbox image (default yuno-sandbox:latest)
//        PROBE_IMAGE          the image the override selects (default yuno-sandbox-node:latest)
//        PROBE_IMAGE_MARKER   a command whose output identifies PROBE_IMAGE (default "node --version")
import crypto from "node:crypto";
import http from "node:http";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const KEEP = process.argv.includes("--keep");
const IMAGE_DEFAULT = process.env.PROBE_IMAGE_DEFAULT || "yuno-sandbox:latest";
const IMAGE = process.env.PROBE_IMAGE || "yuno-sandbox-node:latest";
const MARKER = process.env.PROBE_IMAGE_MARKER || "node --version";
const AGENT = "probesbx-" + crypto.randomBytes(3).toString("hex");
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + detail : ""}`); if (!cond) failures++; };
const show = (label, v) => console.log(`     ${label}: ${typeof v === "string" ? v : S(v).slice(0, 600)}`);
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }

// The command every session runs: which image (marker), uid, HOME, and whether the
// network is reachable (curl is in every platform sandbox image).
const COMMAND = `echo probe-start; (${MARKER}) 2>&1 | head -1; echo uid=$(id -u); echo home=$HOME; ` +
  `echo caches=$XDG_CACHE_HOME,$GOPATH,$npm_config_cache,$PIP_CACHE_DIR; ` +
  `(curl -sS -m 6 -o /dev/null -w 'net=%{http_code}' https://example.com || echo net=off) 2>&1; echo; echo probe-end`;

// ---- mock OpenAI-compatible model: every turn is one exec call, then "Done." --------
let reqN = 0;
const sse = (res, o) => res.write(`data: ${JSON.stringify(o)}\n\n`);
const mock = http.createServer((req, res) => {
  let body = ""; req.on("data", (c) => (body += c));
  req.on("end", () => {
    if (!req.url.endsWith("/chat/completions")) { res.writeHead(404); return res.end(); }
    let b = {}; try { b = JSON.parse(body); } catch {}
    reqN++;
    const last = b.messages?.[b.messages.length - 1];
    const toolResultSeen = last?.role === "tool";
    const usage = { prompt_tokens: 100 + reqN, completion_tokens: 9, total_tokens: 109 + reqN };
    const id = "chatcmpl-" + reqN;
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    if (!toolResultSeen) {
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: { role: "assistant", tool_calls: [{ index: 0, id: "call_" + reqN, type: "function", function: { name: "exec", arguments: JSON.stringify({ command: COMMAND }) } }] }, finish_reason: null }] });
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage });
    } else {
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: { role: "assistant", content: "Done." }, finish_reason: null }] });
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage });
    }
    res.write("data: [DONE]\n\n"); res.end();
  });
});
await new Promise((r) => mock.listen(9999, "127.0.0.1", r));

// ---- gateway client ----------------------------------------------------------------
function connect() {
  return new Promise((resolve) => {
    const ws = new WebSocket(GW); const api = { ws, events: [], connect: null, closed: null }; let next = 10; const pending = {};
    api.call = (method, params) => new Promise((res) => { const id = String(next++); pending[id] = res; ws.send(JSON.stringify({ type: "req", id, method, params })); });
    const timer = setTimeout(() => { api.connect = { ok: false, error: "timeout" }; ws.close(); }, 15000);
    ws.onclose = (e) => { api.closed = { code: e.code, reason: e.reason }; clearTimeout(timer); resolve(api); };
    ws.onmessage = (m) => {
      const f = JSON.parse(m.data);
      if (f.type === "event" && f.event === "connect.challenge") {
        ws.send(JSON.stringify({ type: "req", id: "1", method: "connect", params: {
          minProtocol: 4, maxProtocol: 4,
          client: { id: "gateway-client", version: "0.0.1", platform: "linux", mode: "backend" },
          role: "operator", scopes: ["operator.read", "operator.write", "operator.admin", "operator.approvals"],
          caps: ["approvals", "exec-approvals"], commands: [], permissions: {}, auth: { token: TOKEN }, locale: "en-US", userAgent: "yuno-probe-sandbox/0.1" } }));
        return;
      }
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error, auth: f.payload?.auth, server: f.payload?.server, methods: f.payload?.features?.methods }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
let api = await connect();
ok("connect over loopback", !!api.connect?.ok, S(api.connect?.error));
if (!api.connect?.ok) { mock.close(); process.exit(1); }
console.log("gateway", api.connect.server?.version);
const methods = api.connect.methods || [];
show("sandbox methods", methods.filter((m) => /sandbox/i.test(m)));

async function patch(obj, replacePaths) {
  const cfg = await api.call("config.get", {});
  const params = { raw: JSON.stringify(obj), baseHash: cfg.payload?.hash };
  if (replacePaths) params.replacePaths = replacePaths;
  return api.call("config.patch", params);
}
const stateDir = process.env.OPENCLAW_STATE_DIR || "/home/node/.openclaw";

// 1. Reload kinds of the paths the platform patches for a run (rule 4).
for (const path of ["agents.entries.main.sandbox", "agents.entries.main.sandbox.scope", "agents.entries.main.sandbox.docker.image", "agents.entries.main.sandbox.docker.network"]) {
  const r = await api.call("config.schema.lookup", { path });
  ok(`config.schema.lookup ${path}`, !!r.ok, S(r.error));
  show("reloadKind", { reloadKind: r.payload?.reloadKind, type: r.payload?.schema?.type ?? r.payload?.node?.type, hint: r.payload?.hint });
}

// 2. Global defaults as the baseline writes them.
let r = await api.call("config.get", {});
const defaults = r.payload?.config?.agents?.defaults?.sandbox;
ok("agents.defaults.sandbox present", !!defaults?.docker?.image, S(defaults));
const defaultImage = defaults?.docker?.image;
const defaultNetwork = defaults?.docker?.network;
ok("default sandbox network is none", defaultNetwork === "none", S(defaultNetwork));

// 3. Provider + agent (as openclaw-probe.mjs does).
r = await patch({ models: { providers: { "probe-mock": { baseUrl: "http://127.0.0.1:9999/v1", apiKey: "mock", api: "openai-completions", models: [{ id: "mock-1", name: "Mock", reasoning: false, input: ["text"], cost: { input: 1, output: 5, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 4096 }] } } } });
ok("config.patch provider (hot)", !!r.ok, S(r.error));
r = await api.call("agents.create", { name: AGENT, workspace: `${stateDir}/workspace-${AGENT}` });
ok("agents.create", !!r.ok || /exists/i.test(S(r.error)), S(r.error));
r = await patch({ agents: { entries: { [AGENT]: { model: { primary: "probe-mock/mock-1" }, thinkingDefault: "off", reasoningDefault: "off", tools: { profile: "coding", exec: { host: "sandbox" } } } } } });
ok("config.patch agent entry", !!r.ok, S(r.error));
await sleep(1500);
if (api.closed) { api = await connect(); ok("reconnect after patch", !!api.connect?.ok); }

// turn runs one exec in a fresh session and returns the command output.
async function turn(label) {
  const key = `agent:${AGENT}:yuno-probe-${label}-${crypto.randomBytes(2).toString("hex")}`;
  let r = await api.call("sessions.messages.subscribe", { key, agentId: AGENT, includeApprovals: true });
  if (!r.ok) { ok(`${label}: subscribe`, false, S(r.error)); return ""; }
  const before = api.events.length;
  r = await api.call("chat.send", { sessionKey: key, agentId: AGENT, message: "please run the command", idempotencyKey: crypto.randomUUID() });
  ok(`${label}: chat.send started`, r.ok && r.payload?.status === "started", S(r.error || r.payload));
  const runId = r.payload?.runId;
  let final = null;
  for (let i = 0; i < 360 && !final; i++) { // the first session pulls and creates a container
    await sleep(500);
    final = api.events.slice(before).find((e) => e.event === "chat" && e.payload?.runId === runId && ["final", "error", "aborted"].includes(e.payload?.state));
    const appr = api.events.slice(before).find((e) => e.event === "exec.approval.requested" && !e.resolved);
    if (appr) { appr.resolved = true; await api.call("exec.approval.resolve", { id: appr.payload?.id ?? appr.payload?.request?.id, decision: "allow-once" }); }
  }
  const evs = api.events.slice(before);
  ok(`${label}: turn reached a terminal state`, !!final, final ? final.payload.state : "no final after 180s");
  const outs = evs.filter((e) => e.event === "agent" && e.payload?.stream === "command_output").map((e) => String(e.payload?.data?.output ?? ""));
  let text = outs.join("\n");
  if (!/probe-end/.test(text)) {
    // Output arrives in chunks or only in the tool result; read the history too.
    const h = await api.call("chat.history", { sessionKey: key, agentId: AGENT, limit: 50 });
    const tr = (h.payload?.messages || []).filter((m) => m.role === "toolResult").map((m) => S(m.content)).join("\n");
    text += "\n" + tr;
  }
  const errs = evs.filter((e) => e.event === "agent" && e.payload?.stream === "item" && e.payload?.data?.phase === "end" && e.payload?.data?.error).map((e) => e.payload.data.error);
  if (errs.length) show(`${label}: tool errors`, errs);
  show(`${label}: output`, text.replace(/\\n/g, " | ").replace(/\n/g, " | ").slice(0, 500));
  await api.call("sessions.delete", { key });
  return text;
}

// 4. Baseline: no override → the default image, no network.
const base = await turn("baseline");
ok("baseline runs in the default image (no marker)", /probe-end/.test(base) && !/^v?\d+\.\d+\.\d+/m.test(base.split("probe-start")[1]?.split("\n")[1] || "") || !new RegExp(MARKER.split(" ")[0]).test(base) || /not found/.test(base), "");
ok("baseline has no network", /net=off|net=000/.test(base), "");

// 5. Override: a per-agent sandbox block with the toolchain image and bridge network,
//    scope session so the next session gets a fresh container.
r = await patch({ agents: { entries: { [AGENT]: { sandbox: { scope: "session", docker: { image: IMAGE, network: "bridge" } } } } } });
ok("config.patch agents.entries.<id>.sandbox (image + network)", !!r.ok, S(r.error));
await sleep(1000);
if (api.closed) { api = await connect(); ok("reconnect after sandbox patch", !!api.connect?.ok); }
r = await api.call("config.get", {});
const live = r.payload?.config?.agents?.entries?.[AGENT]?.sandbox;
ok("entry carries the sandbox block", live?.docker?.image === IMAGE && live?.docker?.network === "bridge" && live?.scope === "session", S(live));
ok("defaults untouched by the per-agent block", r.payload?.config?.agents?.defaults?.sandbox?.docker?.image === defaultImage && r.payload?.config?.agents?.defaults?.sandbox?.docker?.network === defaultNetwork);
const over = await turn("override");
ok(`override session runs in ${IMAGE} (marker "${MARKER}" answered)`, /probe-end/.test(over) && !/not found/.test(over) && /probe-start[\s\S]*?(v?\d+\.\d+|go version|Python)/.test(over), "");
ok("override session has network", /net=200|net=\d{3}/.test(over) && !/net=off|net=000/.test(over), "");
ok("override session runs as uid 1000", /uid=1000/.test(over), "");
ok("HOME is the workspace (the gateway sets HOME=/workspace for exec, whatever the image says)", /home=\/workspace$/m.test(over), "");
ok("the image's cache variables survive the gateway's exec environment", /caches=\/workspace\/\.cache,/.test(over), "");

// 6. Restore: delete the block (null, listed in replacePaths) → defaults again.
r = await patch({ agents: { entries: { [AGENT]: { sandbox: null } } } }, [`agents.entries.${AGENT}.sandbox`]);
ok("config.patch removes the sandbox block", !!r.ok, S(r.error));
await sleep(1000);
if (api.closed) { api = await connect(); ok("reconnect after restore", !!api.connect?.ok); }
r = await api.call("config.get", {});
ok("entry has no sandbox block after the restore", r.payload?.config?.agents?.entries?.[AGENT]?.sandbox === undefined, S(r.payload?.config?.agents?.entries?.[AGENT]?.sandbox));
const after = await turn("restored");
ok("restored session has no network again", /net=off|net=000/.test(after), "");

// 7. sandbox.recreate and sandbox.containers, if offered: what the api could call
//    without a Docker socket of its own.
if (methods.includes("sandbox.containers")) {
  r = await api.call("sandbox.containers", {});
  ok("sandbox.containers answers", !!r.ok, S(r.error));
  show("containers for the probe agent", (r.payload?.containers || r.payload || []).filter?.((c) => S(c).includes(AGENT)) ?? r.payload);
}

// Cleanup
if (!KEEP) {
  r = await api.call("agents.delete", { agentId: AGENT });
  ok("agents.delete", !!r.ok, S(r.error));
  r = await patch({ models: { providers: { "probe-mock": null } } }, ["models.providers.probe-mock", "models.providers.probe-mock.models"]);
  ok("provider removed", !!r.ok, S(r.error));
}
mock.close();
console.log(failures === 0 ? "probe-sandbox: all checks passed" : `probe-sandbox: ${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
