#!/usr/bin/env node
// Telegram channel probe (DESIGN 5.8): binds one managed agent to the Telegram channel,
// watches the gateway route an inbound DM to that agent's session and reply, then
// raises a plugin approval on that session to see whether it reaches the chat. The
// human sends the messages; this script only configures, observes and prints the
// payload shapes the engine must handle. Runs in the gateway's network namespace
// (compose service `probe`). Exit 0 when every check passed.
//
// Before anything else it prints, read-only: the gateway's restart/config/channel/session
// RPCs, the reload kind the gateway assigns to every path the platform patches
// (config.schema.lookup), the Telegram channel status (bot username, mode, running),
// the Telegram sessions with their key format, and the restart preflight.
//
// --no-bind observes the existing binding and never writes config. --wait-s 0 skips the
// DM wait. --restart (only with --no-bind) asks the gateway for its safe restart
// (gateway.restart.request) and checks that the process restarts in place: the
// connection drops, a new one is accepted with a new connId and uptime, and the config
// (bindings included) is unchanged. Run it only when no turn is in flight.
//
// --approval also raises a plugin approval on the Telegram session (step 3), which the
// bot delivers to the chat when plugin approvals route there.
//
// Usage: node scripts/openclaw-probe-telegram.mjs [--agent <openclaw agent id>] [--wait-s 120] [--keep]
//          [--no-bind] [--restart] [--approval] [--approval-wait-s 90] [--dm-scope per-channel-peer]
//   (env: OPENCLAW_GATEWAY_TOKEN; --agent is required unless --no-bind finds the bound agent)
import crypto from "node:crypto";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const arg = (name, def) => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : def; };
let AGENT = arg("--agent", "");
const WAIT_S = Number(arg("--wait-s", "120"));
const KEEP = process.argv.includes("--keep");
const NO_BIND = process.argv.includes("--no-bind"); // observe an existing binding (e.g. after a gateway restart)
const DM_SCOPE = arg("--dm-scope", "per-channel-peer");
const RESTART = process.argv.includes("--restart");
// The plugin approval in step 3 is delivered to the chat when routing works, which is a
// message from the bot; it only runs when asked for.
const APPROVAL = process.argv.includes("--approval");
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mask = (s) => String(s).replace(/[0-9]{8,12}:[A-Za-z0-9_-]{30,}/g, "<token>");
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + mask(detail) : ""}`); if (!cond) failures++; };
const show = (label, v) => console.log(`     ${label}: ${mask(S(v)).slice(0, 1400)}`);
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }
if (!AGENT && !NO_BIND) { console.error("--agent <openclaw agent id> is required"); process.exit(2); }
if (RESTART && !NO_BIND) { console.error("--restart is only allowed with --no-bind"); process.exit(2); }

function connect() {
  return new Promise((resolve) => {
    const ws = new WebSocket(GW); const api = { ws, events: [], connect: null }; let next = 10; const pending = {};
    api.call = (method, params) => new Promise((res) => { const id = String(next++); pending[id] = res; ws.send(JSON.stringify({ type: "req", id, method, params })); });
    const timer = setTimeout(() => { api.connect = { ok: false, error: "timeout" }; ws.close(); }, 15000);
    ws.onclose = (e) => { api.closed = { at: Date.now(), code: e.code, reason: e.reason }; clearTimeout(timer); resolve(api); };
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
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error, hello: f.payload }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
let api = await connect();
ok("connect over loopback", !!api.connect?.ok, S(api.connect?.error));
if (!api.connect?.ok) process.exit(1);
async function patch(obj, replacePaths) {
  const cfg = await api.call("config.get", {});
  const params = { raw: JSON.stringify(obj), baseHash: cfg.payload?.hash };
  if (replacePaths) params.replacePaths = replacePaths;
  return api.call("config.patch", params);
}
const kinds = (since) => S(api.events.slice(since).filter((e) => !["tick", "presence", "heartbeat", "health"].includes(e.event)).map((e) => e.event + ":" + (e.payload?.stream || e.payload?.state || "")));


// 0. Read-only inspection: methods, reload kinds, channel status, sessions, preflight.
let r;
const methods = api.connect.hello?.features?.methods ?? [];
show("server", { version: api.connect.hello?.server?.version, connId: api.connect.hello?.server?.connId, methods: methods.length });
show("restart/config/channel RPCs offered", methods.filter((m) => /^(gateway\.|config\.|channels\.)/.test(m)).sort());
show("session/approval RPCs offered", methods.filter((m) => /^(sessions\.(subscribe|list|messages|get|resolve)|plugin\.approval|exec\.approval)/.test(m)).sort());
for (const name of ["gateway.reload", "gateway.reload.mode", "gateway.restart", "gateway.restart.safe"]) {
  console.log(`     method ${name}: ${methods.includes(name) ? "offered" : "not offered"}`);
}
for (const path of ["bindings", "channels.telegram", "channels.telegram.enabled", "channels.telegram.dmPolicy", "channels.telegram.groupPolicy", "session.dmScope", "agents.entries.main.model", "gateway.reload.mode"]) {
  const lk = await api.call("config.schema.lookup", { path });
  const sch = lk.payload?.schema ?? {};
  show(`schema ${path}`, lk.ok ? { reloadKind: lk.payload?.reloadKind ?? lk.payload?.hint?.reloadKind ?? lk.payload?.reload ?? null, enum: sch.enum ?? sch.anyOf?.map((a) => a.const ?? a.enum ?? a.type), keys: Object.keys(lk.payload ?? {}) } : lk.error);
}
// probe: true makes the plugin call Telegram's getMe; the platform reads the bot username
// from it rather than ever handling the token.
r = await api.call("channels.status", { channel: "telegram", probe: true, timeoutMs: 8000 });
ok("channels.status telegram (probe)", !!r.ok, S(r.error));
show("channels.status channels.telegram", r.payload?.channels?.telegram);
show("channels.status channelAccounts.telegram", r.payload?.channelAccounts?.telegram);
r = await api.call("sessions.list", {});
const allSessions = r.payload?.sessions ?? (Array.isArray(r.payload) ? r.payload : []);
const tgSessions = allSessions.filter((x) => /telegram/i.test(S(x)));
show("sessions.list telegram sessions (key, agent, channel, origin)", tgSessions.map((x) => ({ key: x.key, agentId: x.agentId, channel: x.channel ?? x.lastChannel, origin: x.origin, createdVia: x.createdVia, updatedAt: x.updatedAt })));
if (tgSessions[0]) {
  show("first telegram session entry keys", Object.keys(tgSessions[0]));
  show("first telegram session deliveryContext", { deliveryContext: tgSessions[0].deliveryContext, lastChannel: tgSessions[0].lastChannel, lastTo: tgSessions[0].lastTo, displayName: tgSessions[0].displayName });
  // The transcript shape the mirror parses: roles, part types, the __openclaw block and
  // how an inbound channel message is framed (first characters only).
  const h = await api.call("chat.history", { sessionKey: tgSessions[0].key, agentId: tgSessions[0].agentId, limit: 6 });
  show("telegram history entries", (h.payload?.messages ?? []).map((m) => ({
    role: m.role, parts: Array.isArray(m.content) ? m.content.map((p) => p.type) : typeof m.content,
    head: (typeof m.content === "string" ? m.content : (m.content ?? []).filter((p) => p.type === "text").map((p) => p.text).join(" ")).slice(0, 160),
    meta: m.__openclaw ? Object.keys(m.__openclaw) : null, usage: !!m.usage, ts: m.timestamp,
  })));
}
r = await api.call("gateway.restart.preflight", {});
show("gateway.restart.preflight", r.ok ? r.payload : r.error);
const preflight = r.payload;

// 1. Bind the agent to the Telegram channel (DESIGN 5.8 rule 3).
r = await api.call("config.get", {});
const asObj = (c) => (typeof c === "string" ? JSON.parse(c) : (c ?? {}));
const cfg = asObj(r.payload?.config);
const prevBindings = cfg.bindings ?? [];
show("existing bindings", prevBindings);
if (!AGENT) AGENT = prevBindings.find((b) => b.match?.channel === "telegram")?.agentId ?? "";
if (!AGENT) console.log("     no Telegram binding: inbound DMs go to the default agent unless the channel refuses them");
if (!NO_BIND) {
  r = await patch({ bindings: [...prevBindings.filter((b) => b.agentId !== AGENT), { agentId: AGENT, match: { channel: "telegram" }, session: { dmScope: DM_SCOPE } }] }, ["bindings"]);
  ok("bindings patched", !!r.ok, S(r.error));
  await sleep(1500);
}
r = await api.call("config.get", {});
const after = asObj(r.payload?.config);
if (AGENT) ok(NO_BIND ? "binding present" : "binding present after patch", (after.bindings ?? []).some((b) => b.agentId === AGENT && b.match?.channel === "telegram"), S(after.bindings));
show("channels.telegram (masked)", { ...(after.channels?.telegram ?? {}), botToken: after.channels?.telegram?.botToken ? "<masked>" : null });


// 1b. --restart: the gateway's own safe restart. In a container with no supervisor the
// gateway restarts in place (same PID 1, same container); this checks it from outside.
if (RESTART) {
  if (preflight && preflight.safe === false) {
    ok("restart preflight is safe (nothing in flight)", false, S(preflight.summary));
  } else {
    const before = { connId: api.connect.hello?.server?.connId, hash: r.payload?.hash };
    const cfgBefore = await api.call("config.get", {});
    const t0 = Date.now();
    r = await api.call("gateway.restart.request", { reason: "yuno probe: apply bindings" });
    ok("gateway.restart.request accepted", !!r.ok, S(r.error));
    show("gateway.restart.request result", r.payload);
    for (let i = 0; i < 120 && !api.closed; i++) await sleep(250);
    ok("the connection dropped for the restart", !!api.closed, api.closed ? `after ${api.closed.at - t0} ms, code ${api.closed.code}` : "still open after 30 s");
    let again = null;
    for (let i = 0; i < 60; i++) {
      await sleep(1000);
      again = await connect();
      if (again.connect?.ok) break;
      show(`reconnect attempt ${i + 1}`, again.connect?.error);
    }
    ok("reconnected after the restart", !!again?.connect?.ok, S(again?.connect?.error));
    if (again?.connect?.ok) {
      api = again;
      show("reconnected", { afterMs: Date.now() - t0, connId: api.connect.hello?.server?.connId, before: before.connId });
      const cfgAfter = await api.call("config.get", {});
      ok("config unchanged across the restart", cfgAfter.payload?.hash === cfgBefore.payload?.hash, `${cfgBefore.payload?.hash} -> ${cfgAfter.payload?.hash}`);
      const b2 = asObj(cfgAfter.payload?.config).bindings ?? [];
      ok("bindings unchanged across the restart", S(b2) === S(prevBindings), S(b2));
      r = await api.call("channels.status", { channel: "telegram" });
      show("channels.status after restart (masked)", r.payload);
    }
  }
}

// 2. Watch sessions: the human sends a DM now.
r = await api.call("sessions.subscribe", {});
ok("sessions.subscribe", !!r.ok, S(r.error));
r = await api.call("sessions.list", {});
show("sessions.list before", (r.payload?.sessions ?? r.payload ?? []).slice?.(0, 5) ?? r.payload);
if (WAIT_S > 0) console.log(`\n>>> Send a direct message to the bot now (waiting up to ${WAIT_S}s for a Telegram session on agent ${AGENT || "(none bound)"})...\n`);
const since = api.events.length;
let tgKey = null; let tgEv = null;
for (let i = 0; i < WAIT_S * 2 && !tgKey; i++) {
  await sleep(500);
  const ev = api.events.slice(since).find((e) => /telegram/i.test(S(e.payload)) && (e.payload?.sessionKey || e.payload?.key || e.payload?.session?.key));
  if (ev) { tgEv = ev; tgKey = ev.payload?.sessionKey || ev.payload?.key || ev.payload?.session?.key; }
}
if (WAIT_S > 0) ok("a Telegram session event arrived", !!tgKey, tgKey ? "" : kinds(since));
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
}
// With --approval and no DM during the wait, the bound agent's existing Telegram session
// is used.
if (!tgKey && APPROVAL && AGENT) {
  r = await api.call("sessions.list", {});
  const own = (r.payload?.sessions ?? []).find((x) => String(x.key).startsWith(`agent:${AGENT}:telegram:`));
  tgKey = own?.key ?? null;
  ok("the bound agent has a Telegram session to raise the approval on", !!tgKey, tgKey ?? "none: send it a DM first");
}
if (tgKey && APPROVAL) {
  const s3 = api.events.length;
  const waitMs = Number(arg("--approval-wait-s", "90")) * 1000;
  r = await api.call("plugin.approval.request", {
    title: "Probe: deploy release 0.0.1?", description: "This is the Yuno platform checking whether side-effect approvals reach Telegram. Reply allow-once or deny.",
    severity: "info", toolName: "deploy_release", toolCallId: "probe-" + crypto.randomBytes(3).toString("hex"), mcpTool: { server: "yuno", tool: "deploy_release" },
    allowedDecisions: ["allow-once", "deny"], agentId: AGENT, sessionKey: tgKey, timeoutMs: Math.max(waitMs, 10000), twoPhase: true,
  });
  ok("plugin.approval.request with sessionKey", !!r.ok, S(r.error));
  show("request result (deliveryRoute says where it went)", r.payload);
  console.log(`\n>>> If the bot sent you an approval prompt, answer it in Telegram (allow or deny) within ${waitMs / 1000}s...\n`);
  const w = await api.call("plugin.approval.waitDecision", { id: r.payload?.id, timeoutMs: waitMs });
  show("waitDecision result", w.payload || w.error);
  const resolved = api.events.slice(s3).find((e) => e.event === "plugin.approval.resolved" && e.payload?.id === r.payload?.id);
  show("resolved event (resolvedBy tells the surface)", resolved?.payload && { decision: resolved.payload.decision, resolvedBy: resolved.payload.resolvedBy });
  show("approval events seen", api.events.slice(s3).filter((e) => /approval/.test(e.event)).map((e) => e.event));
  // Nobody answering is not a failure of the gateway; it leaves the question open.
  console.log(`     decided from the chat: ${resolved && resolved.payload.resolvedBy != null ? "yes" : "no decision within the wait"}`);
  if (!resolved) {
    const d = await api.call("plugin.approval.resolve", { id: r.payload?.id, decision: "deny" });
    show("cleanup: resolved deny", d.ok ? d.payload : d.error);
  }
}

if (!KEEP && !NO_BIND) {
  r = await patch({ bindings: prevBindings }, ["bindings"]);
  ok("bindings restored", !!r.ok, S(r.error));
}
api.ws.close();
console.log(failures === 0 ? "probe-telegram: all checks passed" : `probe-telegram: ${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
