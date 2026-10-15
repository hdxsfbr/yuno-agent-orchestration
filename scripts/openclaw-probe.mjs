#!/usr/bin/env node
// OpenClaw integration canary (DESIGN 5.14). Runs inside the gateway's network
// namespace (compose service `probe`), so 127.0.0.1 is the gateway and a mock model
// server started here on 127.0.0.1:9999 is reachable by the gateway.
//
// It verifies: loopback token auth grants operator scopes; config.patch hot-reloads a
// provider; agents.create works; a chat.send turn streams events; a tool call to exec
// runs inside a Docker sandbox (uname shows the sandbox, not the gateway); per-message
// usage and cost are reported. Cleans up what it created. Exit 0 on success.
//
// Usage: node scripts/openclaw-probe.mjs [--keep]   (env: OPENCLAW_GATEWAY_TOKEN)
import crypto from "node:crypto";
import http from "node:http";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const KEEP = process.argv.includes("--keep");
const AGENT = "probe";
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + detail : ""}`); if (!cond) failures++; };
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }

// ---- mock OpenAI-compatible model: scripted replies by request index --------------
let reqN = 0;
const script = [
  { tool: { name: "exec", args: { command: "echo hi-from-sandbox; uname -n; id -u; ls /workspace 2>/dev/null | head -3; cat /proc/1/cmdline | tr '\\0' ' '" } } },
  { text: "Done. I ran the command." },
  { text: "Done." }, { text: "Done." },
];
const sse = (res, o) => res.write(`data: ${JSON.stringify(o)}\n\n`);
const mock = http.createServer((req, res) => {
  let body = ""; req.on("data", (c) => (body += c));
  req.on("end", () => {
    if (!req.url.endsWith("/chat/completions")) { res.writeHead(404); return res.end(); }
    let b = {}; try { b = JSON.parse(body); } catch {}
    const step = script[Math.min(reqN, script.length - 1)]; reqN++;
    const usage = { prompt_tokens: 100 + reqN, completion_tokens: 9, total_tokens: 109 + reqN };
    const id = "chatcmpl-" + reqN;
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    if (step.tool) {
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: { role: "assistant", tool_calls: [{ index: 0, id: "call_" + reqN, type: "function", function: { name: step.tool.name, arguments: JSON.stringify(step.tool.args) } }] }, finish_reason: null }] });
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage });
    } else {
      sse(res, { id, object: "chat.completion.chunk", created: 0, model: b.model, choices: [{ index: 0, delta: { role: "assistant", content: step.text }, finish_reason: null }] });
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
          caps: ["approvals", "exec-approvals"], commands: [], permissions: {}, auth: { token: TOKEN }, locale: "en-US", userAgent: "yuno-probe/0.1" } }));
        return;
      }
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error, auth: f.payload?.auth, server: f.payload?.server }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
let api = await connect();
ok("connect over loopback", !!api.connect?.ok, S(api.connect?.error));
const scopes = api.connect?.auth?.scopes || [];
ok("operator scopes granted", scopes.includes("operator.admin") && scopes.includes("operator.approvals"), scopes.join(","));
if (!api.connect?.ok) { mock.close(); process.exit(1); }
console.log("gateway", api.connect.server?.version);

async function patch(obj, replacePaths) {
  const cfg = await api.call("config.get", {});
  const params = { raw: JSON.stringify(obj), baseHash: cfg.payload?.hash };
  if (replacePaths) params.replacePaths = replacePaths; // required when removing array-bearing paths
  return api.call("config.patch", params);
}
const stateDir = process.env.OPENCLAW_STATE_DIR || "/home/node/.openclaw";

// Provider + agent
let r = await patch({ models: { providers: { "probe-mock": { baseUrl: "http://127.0.0.1:9999/v1", apiKey: "mock", api: "openai-completions", models: [{ id: "mock-1", name: "Mock", reasoning: false, input: ["text"], cost: { input: 1, output: 5, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 4096 }] } } } });
ok("config.patch provider (hot)", !!r.ok, S(r.error));
r = await api.call("agents.create", { name: AGENT, workspace: `${stateDir}/workspace-${AGENT}` });
const created = !!r.ok || /exists/i.test(S(r.error));
ok("agents.create", created, S(r.error));
r = await patch({ agents: { entries: { [AGENT]: { model: { primary: "probe-mock/mock-1" }, thinkingDefault: "off", reasoningDefault: "off", tools: { profile: "coding", exec: { host: "sandbox" } } } } } });
ok("config.patch agent entry", !!r.ok, S(r.error));
await sleep(1500);
if (api.closed) { api = await connect(); ok("reconnect after patch", !!api.connect?.ok); }

// Turn
const key = `agent:${AGENT}:main`;
r = await api.call("sessions.messages.subscribe", { key, agentId: AGENT, includeApprovals: true });
ok("sessions.messages.subscribe", !!r.ok, S(r.error));
const before = api.events.length;
r = await api.call("chat.send", { sessionKey: key, agentId: AGENT, message: "please run the command", idempotencyKey: crypto.randomUUID() });
ok("chat.send started", r.ok && r.payload?.status === "started", S(r.error || r.payload));
const runId = r.payload?.runId;
let final = null;
for (let i = 0; i < 240 && !final; i++) { // sandbox creation can take a while the first time
  await sleep(500);
  final = api.events.slice(before).find((e) => e.event === "chat" && e.payload?.runId === runId && ["final", "error", "aborted"].includes(e.payload?.state));
  const appr = api.events.slice(before).find((e) => e.event === "exec.approval.requested" && !e.resolved);
  if (appr) { appr.resolved = true; const rr = await api.call("exec.approval.resolve", { id: appr.payload?.id ?? appr.payload?.request?.id, decision: "allow-once" }); console.log("     resolved exec approval:", S(rr.ok ? rr.payload : rr.error).slice(0, 200)); }
}
const evs = api.events.slice(before).filter((e) => !["tick", "presence", "heartbeat", "health"].includes(e.event));
ok("turn reached a terminal state", !!final, final ? final.payload.state : `no final after 120s; ${evs.length} events`);
const items = evs.filter((e) => e.event === "agent" && e.payload?.stream === "item" && e.payload?.data?.name === "exec");
const ended = items.find((e) => e.payload.data.phase === "end");
ok("exec tool call observed", items.length > 0, `${items.length} item events`);
ok("exec succeeded", ended?.payload?.data?.status === "ok" || ended?.payload?.data?.status === "completed" || ended?.payload?.data?.status === "success", S(ended?.payload?.data));
const cmdOut = evs.find((e) => e.event === "agent" && e.payload?.stream === "command_output");
const outText = String(cmdOut?.payload?.data?.output ?? "");
console.log("     command output:", outText.replace(/\n/g, " | ").slice(0, 400));
ok("command ran in the sandbox (not the gateway)", /hi-from-sandbox/.test(outText) && !/openclaw\.mjs gateway/.test(outText), "");
const usageMsg = evs.find((e) => e.event === "session.message" && e.payload?.message?.role === "assistant" && e.payload?.message?.usage);
ok("usage and cost on assistant message", !!usageMsg, S(usageMsg?.payload?.message?.usage));
const reasoning = evs.find((e) => e.event === "agent" && e.payload?.stream === "reasoning");
ok("no reasoning stream emitted", !reasoning);
console.log("     event kinds:", S(evs.reduce((m, e) => { const k = e.event + ":" + (e.payload?.stream || e.payload?.state || ""); m[k] = (m[k] || 0) + 1; return m; }, {})));

// Cleanup
if (!KEEP) {
  r = await api.call("agents.delete", { agentId: AGENT });
  ok("agents.delete", !!r.ok, S(r.error));
  r = await patch({ models: { providers: { "probe-mock": null } } }, ["models.providers.probe-mock", "models.providers.probe-mock.models"]);
  ok("provider removed", !!r.ok, S(r.error));
}
mock.close();
console.log(failures === 0 ? "probe: all checks passed" : `probe: ${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
