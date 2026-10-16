#!/usr/bin/env node
// Telegram channel probe (DESIGN 5.8): binds one managed agent to the Telegram channel,
// watches the gateway route an inbound DM to that agent's session and reply, then
// raises a plugin approval on that session to see whether it reaches the chat. The
// human sends the messages; this script only configures, observes and prints the
// payload shapes the engine must handle. Runs in the gateway's network namespace
// (compose service `probe`). Exit 0 when every check passed.
//
// Usage: node scripts/openclaw-probe-telegram.mjs --agent <openclaw agent id> [--wait-s 120] [--keep] [--no-bind] [--dm-scope per-channel-peer]
//   (env: OPENCLAW_GATEWAY_TOKEN)
import crypto from "node:crypto";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const arg = (name, def) => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : def; };
const AGENT = arg("--agent", "");
const WAIT_S = Number(arg("--wait-s", "120"));
const KEEP = process.argv.includes("--keep");
const NO_BIND = process.argv.includes("--no-bind"); // observe an existing binding (e.g. after a gateway restart)
const DM_SCOPE = arg("--dm-scope", "per-channel-peer");
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mask = (s) => String(s).replace(/[0-9]{8,12}:[A-Za-z0-9_-]{30,}/g, "<token>");
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + mask(detail) : ""}`); if (!cond) failures++; };
const show = (label, v) => console.log(`     ${label}: ${mask(S(v)).slice(0, 1400)}`);
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }
if (!AGENT) { console.error("--agent <openclaw agent id> is required"); process.exit(2); }

function connect() {
  return new Promise((resolve) => {
    const ws = new WebSocket(GW); const api = { ws, events: [], connect: null }; let next = 10; const pending = {};
    api.call = (method, params) => new Promise((res) => { const id = String(next++); pending[id] = res; ws.send(JSON.stringify({ type: "req", id, method, params })); });
    const timer = setTimeout(() => { api.connect = { ok: false, error: "timeout" }; ws.close(); }, 15000);
    ws.onclose = () => { clearTimeout(timer); resolve(api); };
    ws.onmessage = (m) => {
      const f = JSON.parse(m.data);
      if (f.type === "event" && f.event === "connect.challenge") {
        ws.send(JSON.stringify({ type: "req", id: "1", method: "connect", params: {
          minProtocol: 4, maxProtocol: 4,
          client: { id: "gateway-client", version: "0.0.1", platform: "linux", mode: "backend" },
          role: "operator", scopes: ["operator.read", "operator.write", "operator.admin", "operator.approvals"],
          caps: ["approvals", "exec-approvals"], commands: [], permissions: {}, auth: { token: TOKEN }, locale: "en-US", userAgent: "yuno-probe-telegram/0.1" } }));
        return;
      }
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
const api = await connect();
ok("connect over loopback", !!api.connect?.ok, S(api.connect?.error));
if (!api.connect?.ok) process.exit(1);
async function patch(obj, replacePaths) {
  const cfg = await api.call("config.get", {});
  const params = { raw: JSON.stringify(obj), baseHash: cfg.payload?.hash };
  if (replacePaths) params.replacePaths = replacePaths;
  return api.call("config.patch", params);
}
const kinds = (since) => S(api.events.slice(since).filter((e) => !["tick", "presence", "heartbeat", "health"].includes(e.event)).map((e) => e.event + ":" + (e.payload?.stream || e.payload?.state || "")));

// 1. Bind the agent to the Telegram channel (DESIGN 5.8 rule 3).
let r = await api.call("config.get", {});
const asObj = (c) => (typeof c === "string" ? JSON.parse(c) : (c ?? {}));
const cfg = asObj(r.payload?.config);
const prevBindings = cfg.bindings ?? [];
show("existing bindings", prevBindings);
if (!NO_BIND) {
  r = await patch({ bindings: [...prevBindings.filter((b) => b.agentId !== AGENT), { agentId: AGENT, match: { channel: "telegram" }, session: { dmScope: DM_SCOPE } }] }, ["bindings"]);
  ok("bindings patched", !!r.ok, S(r.error));
  await sleep(1500);
}
r = await api.call("config.get", {});
const after = asObj(r.payload?.config);
ok("binding present after patch", (after.bindings ?? []).some((b) => b.agentId === AGENT && b.match?.channel === "telegram"), S(after.bindings));
show("channels.telegram (masked)", { ...(after.channels?.telegram ?? {}), botToken: after.channels?.telegram?.botToken ? "<masked>" : null });

// 2. Watch sessions: the human sends a DM now.
r = await api.call("sessions.subscribe", {});
ok("sessions.subscribe", !!r.ok, S(r.error));
r = await api.call("sessions.list", {});
show("sessions.list before", (r.payload?.sessions ?? r.payload ?? []).slice?.(0, 5) ?? r.payload);
console.log(`\n>>> Send a direct message to the bot now (waiting up to ${WAIT_S}s for a Telegram session on agent ${AGENT})...\n`);
const since = api.events.length;
let tgKey = null; let tgEv = null;
for (let i = 0; i < WAIT_S * 2 && !tgKey; i++) {
  await sleep(500);
  const ev = api.events.slice(since).find((e) => /telegram/i.test(S(e.payload)) && (e.payload?.sessionKey || e.payload?.key || e.payload?.session?.key));
  if (ev) { tgEv = ev; tgKey = ev.payload?.sessionKey || ev.payload?.key || ev.payload?.session?.key; }
}
ok("a Telegram session event arrived", !!tgKey, tgKey ? "" : kinds(since));
show("first Telegram event", tgEv);
if (tgKey) {
  ok("the session belongs to the bound agent", String(tgKey).startsWith(`agent:${AGENT}:`), tgKey);
  r = await api.call("sessions.messages.subscribe", { key: tgKey, agentId: AGENT, includeApprovals: true });
  ok("sessions.messages.subscribe on the Telegram session", !!r.ok, S(r.error));
  // wait for the reply to the human
  const s2 = api.events.length;
  let replied = null;
  for (let i = 0; i < 240 && !replied; i++) {
    await sleep(500);
    replied = api.events.slice(s2).find((e) => e.event === "session.message" && e.payload?.message?.role === "assistant") || api.events.slice(since).find((e) => e.event === "chat" && ["final", "error", "aborted"].includes(e.payload?.state));
  }
  ok("the agent produced a reply (assistant message or final chat state)", !!replied, replied ? "" : kinds(s2));
  show("reply event", replied);
  r = await api.call("chat.history", { sessionKey: tgKey, limit: 6 });
  ok("chat.history on the Telegram session", !!r.ok, S(r.error));
  show("history (last entries, roles and lengths)", (r.payload?.messages ?? []).slice(-6).map((m) => ({ role: m.role, len: S(m.content).length, usage: m.usage ? "yes" : "no" })));
  show("event kinds during the turn", kinds(since));

  // 3. A plugin approval tied to this session: does it reach the chat?
  const s3 = api.events.length;
  r = await api.call("plugin.approval.request", {
    title: "Probe: deploy release 0.0.1?", description: "This is the Yuno platform checking whether side-effect approvals reach Telegram. Reply allow-once or deny.",
    severity: "info", toolName: "deploy_release", toolCallId: "probe-" + crypto.randomBytes(3).toString("hex"), mcpTool: { server: "yuno", tool: "deploy_release" },
    allowedDecisions: ["allow-once", "deny"], agentId: AGENT, sessionKey: tgKey, timeoutMs: 90000, twoPhase: true,
  });
  ok("plugin.approval.request with sessionKey", !!r.ok, S(r.error));
  show("request result (deliveryRoute says where it went)", r.payload);
  console.log("\n>>> If the bot sent you an approval prompt, answer it in Telegram (allow or deny) within 90s...\n");
  const w = await api.call("plugin.approval.waitDecision", { id: r.payload?.id, timeoutMs: 90000 });
  show("waitDecision result", w.payload || w.error);
  const resolved = api.events.slice(s3).find((e) => e.event === "plugin.approval.resolved" && e.payload?.id === r.payload?.id);
  show("resolved event (resolvedBy tells the surface)", resolved?.payload && { decision: resolved.payload.decision, resolvedBy: resolved.payload.resolvedBy });
  ok("approval decided from the chat (resolvedBy not null, not timeout)", !!resolved && resolved.payload.resolvedBy != null, resolved ? "" : "no resolved event");
}

if (!KEEP && !NO_BIND) {
  r = await patch({ bindings: prevBindings }, ["bindings"]);
  ok("bindings restored", !!r.ok, S(r.error));
}
api.ws.close();
console.log(failures === 0 ? "probe-telegram: all checks passed" : `probe-telegram: ${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
