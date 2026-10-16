#!/usr/bin/env node
// Report-back probe (DESIGN wave 5, Chief of Staff rule 9): can the platform inject a
// message into a Telegram session with chat.send {deliver: true} so that the agent's
// reply is delivered to the Telegram chat, and does the injected text appear as a user
// turn in the transcript? Runs in the gateway's network namespace (compose service
// `probe`). The human checks the phone; this script observes events and the history.
// It sends ONE message through the bot, so run it only when that is wanted.
//
// Usage: node scripts/openclaw-probe-deliver.mjs [--session <key>] [--wait-s 90] [--text "<message>"]
//   (env: OPENCLAW_GATEWAY_TOKEN; without --session the most recently updated Telegram direct session is used)
import crypto from "node:crypto";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const arg = (name, def) => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : def; };
const WAIT_S = Number(arg("--wait-s", "90"));
let KEY = arg("--session", "");
const TEXT = arg("--text", "[platform] Probe: run r_probe123 succeeded after 12 s, cost $0.00: the delegated task finished with the output \"hello from the platform\". Reply to the person with one short sentence that relays this outcome.");
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mask = (s) => String(s).replace(/[0-9]{8,12}:[A-Za-z0-9_-]{30,}/g, "<token>").replace(/telegram:direct:[0-9]+/g, "telegram:direct:<id>").replace(/"chatId":"?[0-9]+"?/g, '"chatId":"<id>"');
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + mask(detail) : ""}`); if (!cond) failures++; };
const show = (label, v) => console.log(`     ${label}: ${mask(S(v)).slice(0, 1200)}`);
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }

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
          caps: ["approvals", "exec-approvals"], commands: [], permissions: {}, auth: { token: TOKEN }, locale: "en-US", userAgent: "yuno-probe-deliver/0.1" } }));
        return;
      }
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error, hello: f.payload }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
const api = await connect();
ok("connect over loopback", !!api.connect?.ok, S(api.connect?.error));
if (!api.connect?.ok) process.exit(1);

// 1. The Telegram session.
let r = await api.call("sessions.list", {});
ok("sessions.list", !!r.ok, S(r.error));
const sessions = r.payload?.sessions ?? r.payload ?? [];
const tg = sessions.filter((x) => /:telegram:direct:/.test(x.key || "")).sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0));
if (!KEY) KEY = tg[0]?.key || "";
ok("a Telegram direct session exists", !!KEY, KEY ? mask(KEY) : "none; chat with the bot first");
if (!KEY) process.exit(1);
const sess = sessions.find((x) => x.key === KEY) || {};
show("session (agent, channel, deliveryContext)", { agentId: sess.agentId, channel: sess.channel ?? sess.lastChannel, lastChannel: sess.lastChannel, deliveryContext: sess.deliveryContext });
const AGENT = sess.agentId;
const before = await api.call("chat.history", { sessionKey: KEY, agentId: AGENT, limit: 50 });
const beforeN = (before.payload?.messages ?? []).length;
ok("chat.history before", !!before.ok, `${beforeN} messages`);

// 2. Subscribe and inject.
r = await api.call("sessions.messages.subscribe", { key: KEY, agentId: AGENT, includeApprovals: true }); // includeApprovals must be true [probed]
ok("sessions.messages.subscribe", !!r.ok, S(r.error));
const since = api.events.length;
const idem = "probe-" + crypto.randomUUID();
r = await api.call("chat.send", { sessionKey: KEY, agentId: AGENT, message: TEXT, idempotencyKey: idem, deliver: true });
ok("chat.send {deliver: true} accepted", !!r.ok, S(r.ok ? { runId: r.payload?.runId, status: r.payload?.status } : r.error));
if (!r.ok) process.exit(1);
const runId = r.payload?.runId;

// 3. Wait for the turn to end. Chat events for the run reach this connection once it is
// subscribed to the session [probed]; completion is still read from the transcript (a new
// assistant entry after the injected user turn) so the check does not depend on them.
const deadline = Date.now() + WAIT_S * 1000;
let ended = false;
while (Date.now() < deadline) {
  const h = await api.call("chat.history", { sessionKey: KEY, agentId: AGENT, limit: 50 });
  const m = h.payload?.messages ?? [];
  if (m.length >= beforeN + 2 && m[m.length - 1]?.role === "assistant") { ended = true; break; }
  await sleep(1000);
}
ok("chat turn ended (assistant entry in the transcript)", ended, ended ? "" : `no assistant reply within ${WAIT_S}s`);
const chatEv = api.events.slice(since).filter((e) => e.event === "chat" && e.payload?.runId === runId);
show("chat events for this run on this connection", chatEv.length);
const kinds = {};
for (const e of api.events.slice(since)) kinds[e.event] = (kinds[e.event] || 0) + 1;
show("events seen after chat.send", kinds);
const delivery = api.events.slice(since).filter((e) => /delivery|outbound|send/i.test(e.event) || /delivery/i.test(S(e.payload)));
show("delivery-related events (first 3)", delivery.slice(0, 3).map((e) => ({ event: e.event, payload: e.payload })));
const sessMsgs = api.events.slice(since).filter((e) => e.event === "session.message").map((e) => ({ role: e.payload?.message?.role ?? e.payload?.role, text: String(e.payload?.message?.content?.[0]?.text ?? e.payload?.message?.text ?? e.payload?.text ?? "").slice(0, 100) }));
show("session.message events (role, text)", sessMsgs);

// 4. The transcript: the injected text as a user turn, then the assistant reply.
await sleep(1500);
const after = await api.call("chat.history", { sessionKey: KEY, agentId: AGENT, limit: 50 });
const msgs = after.payload?.messages ?? [];
const tail = msgs.slice(beforeN).map((m) => ({ role: m.role, text: String((Array.isArray(m.content) ? m.content.map((c) => c.text || "").join(" ") : m.content) || "").slice(0, 120) }));
show("new transcript entries", tail);
const injected = tail.find((m) => m.role === "user" && m.text.startsWith("[platform] Probe"));
ok("injected text is a user turn in the transcript", !!injected);
const reply = tail.find((m) => m.role === "assistant" && m.text.length > 0);
ok("assistant replied in the session", !!reply, reply ? reply.text : "");
console.log("");
console.log("Check the phone: the reply above should have arrived as a Telegram message from the bot.");
console.log(failures ? `FAILED (${failures})` : "all gateway-side checks passed; the Telegram delivery is confirmed by the human");
api.ws.close();
process.exit(failures ? 1 : 0);
