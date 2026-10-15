#!/usr/bin/env node
// Plugin approval probe (DESIGN 5.3 rule 5): verifies the gateway RPCs the platform
// uses to register a side-effect approval so OpenClaw's own surfaces (Control UI,
// Telegram) can answer it, and prints every payload shape the engine must parse.
//
// Two operator connections play the two roles: A is the api (requests and waits),
// B is another approval client (lists and resolves). Runs inside the gateway's
// network namespace (compose service `probe`). Exit 0 when every check passed.
//
// Usage: node scripts/openclaw-probe-approval.mjs [--timeout-ms N]   (env: OPENCLAW_GATEWAY_TOKEN)
import crypto from "node:crypto";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN;
const GW = process.env.OPENCLAW_GATEWAY_URL || "ws://127.0.0.1:18789";
const argIdx = process.argv.indexOf("--timeout-ms");
const SHORT_TIMEOUT = argIdx > 0 ? Number(process.argv[argIdx + 1]) : 1500;
const S = (v) => String(JSON.stringify(v ?? null));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (name, cond, detail = "") => { console.log(`${cond ? "ok  " : "FAIL"}  ${name}${detail ? ": " + detail : ""}`); if (!cond) failures++; };
const show = (label, v) => console.log(`     ${label}: ${S(v).slice(0, 1200)}`);
if (!TOKEN) { console.error("OPENCLAW_GATEWAY_TOKEN is required"); process.exit(2); }

function connect(name) {
  return new Promise((resolve) => {
    const ws = new WebSocket(GW); const api = { name, ws, events: [], connect: null, closed: null }; let next = 10; const pending = {};
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
          caps: ["approvals", "exec-approvals"], commands: [], permissions: {}, auth: { token: TOKEN }, locale: "en-US", userAgent: "yuno-probe-approval/" + name } }));
        return;
      }
      if (f.type === "res" && f.id === "1") { api.connect = { ok: f.ok, error: f.error, auth: f.payload?.auth }; clearTimeout(timer); if (!f.ok) ws.close(); else resolve(api); return; }
      if (f.type === "res") { const r = pending[f.id]; delete pending[f.id]; r && r({ ok: f.ok, error: f.error, payload: f.payload }); return; }
      if (f.type === "event") api.events.push({ t: Date.now(), event: f.event, payload: f.payload });
    };
  });
}
const A = await connect("A");
const B = await connect("B");
ok("two operator connections", !!A.connect?.ok && !!B.connect?.ok, S(A.connect?.error || B.connect?.error));
if (!A.connect?.ok || !B.connect?.ok) process.exit(1);
const evs = (c, name, since = 0) => c.events.slice(since).filter((e) => e.event === name);
const kinds = (c, since) => S(c.events.slice(since).filter((e) => !["tick", "presence", "heartbeat", "health"].includes(e.event)).map((e) => e.event));

// 1. Without twoPhase the request call itself blocks until the decision or the timeout.
const callId = "probe-" + crypto.randomBytes(4).toString("hex");
const base = {
  title: "Deploy release 1.2.3?",
  description: "deploy_release for run 01a1-probe by agent Deployer: version 1.2.3, 2 files",
  detail: '{"version":"1.2.3","paths":["runs/x/app.py","runs/x/README.md"]}',
  severity: "warning",
  toolName: "deploy_release",
  toolCallId: callId,
  mcpTool: { server: "yuno", tool: "deploy_release" },
  allowedDecisions: ["allow-once", "deny"],
};
let t0 = Date.now();
let r = await A.call("plugin.approval.request", { ...base, toolCallId: callId + "-blocking", timeoutMs: SHORT_TIMEOUT });
let elapsed = Date.now() - t0;
ok("one-phase request blocks until the gateway timeout", !!r.ok && elapsed >= SHORT_TIMEOUT - 50, `${elapsed} ms, ${S(r.error)}`);
show("one-phase result after timeout", r.payload);
ok("one-phase timeout result has decision null (no terminalReason field)", r.payload?.decision === null && !("terminalReason" in (r.payload || {})));

// 2. twoPhase: the request returns at once with the id; the decision comes later.
let sinceA = A.events.length, sinceB = B.events.length;
t0 = Date.now();
r = await A.call("plugin.approval.request", { ...base, twoPhase: true, timeoutMs: 60000 });
elapsed = Date.now() - t0;
ok("two-phase request returns at once", !!r.ok && elapsed < 2000, `${elapsed} ms, ${S(r.error)}`);
show("two-phase request result", r.payload);
const id = r.payload?.id;
ok("request result carries an id", typeof id === "string" && id.startsWith("plugin:"), S(id));
await sleep(700);
const reqB = evs(B, "plugin.approval.requested", sinceB)[0];
ok("requested event reaches the other approval client", !!reqB, reqB ? "" : kinds(B, sinceB));
show("requested event (B)", reqB?.payload);
show("event kinds seen by A", kinds(A, sinceA));

// 3. List from B while pending.
r = await B.call("plugin.approval.list", {});
ok("plugin.approval.list", !!r.ok, S(r.error));
show("list result", r.payload);
ok("list includes the pending approval", S(r.payload).includes(String(id)));

// 4. A waits for the decision while B resolves it.
sinceA = A.events.length; sinceB = B.events.length;
const waiting = A.call("plugin.approval.waitDecision", { id, timeoutMs: 10000 });
await sleep(400);
r = await B.call("plugin.approval.resolve", { id, decision: "allow-once" });
ok("plugin.approval.resolve allow-once", !!r.ok, S(r.error));
show("resolve result", r.payload);
const w = await waiting;
ok("waitDecision returns the decision", !!w.ok && w.payload?.decision === "allow-once", S(w.error || w.payload));
show("waitDecision result", w.payload);
await sleep(700);
const resA = evs(A, "plugin.approval.resolved", sinceA).find((e) => e.payload?.id === id);
ok("resolved event reaches the requester with the decision", resA?.payload?.decision === "allow-once", resA ? "" : kinds(A, sinceA));
show("resolved event (A)", resA?.payload);
ok("list no longer includes it", !S((await B.call("plugin.approval.list", {})).payload).includes(String(id)));

// 5. A second decision on the same approval.
r = await B.call("plugin.approval.resolve", { id, decision: "deny" });
ok("second decision is refused", !r.ok, S(r.error || r.payload));

// 6. Timeout path with a short timeoutMs, two-phase.
sinceA = A.events.length;
r = await A.call("plugin.approval.request", { ...base, toolCallId: callId + "-t", twoPhase: true, timeoutMs: SHORT_TIMEOUT });
const id2 = r.payload?.id;
const w2 = await A.call("plugin.approval.waitDecision", { id: id2, timeoutMs: SHORT_TIMEOUT + 4000 });
ok("waitDecision after gateway timeout says terminalReason timeout", w2.ok && w2.payload?.terminalReason === "timeout", S(w2.error || w2.payload));
await sleep(700);
const tEv = evs(A, "plugin.approval.resolved", sinceA).find((e) => e.payload?.id === id2);
ok("timeout produces a resolved event", !!tEv, tEv ? "" : kinds(A, sinceA));
show("timeout event payload (decision is deny, resolvedBy null)", { decision: tEv?.payload?.decision, resolvedBy: tEv?.payload?.resolvedBy, ts: tEv?.payload?.ts });
ok("timeout event decision is deny with resolvedBy null", tEv?.payload?.decision === "deny" && tEv?.payload?.resolvedBy === null);

// 7. Deny path, decided from B, observed by A through the event.
sinceA = A.events.length;
r = await A.call("plugin.approval.request", { ...base, toolCallId: callId + "-d", twoPhase: true, timeoutMs: 60000 });
const id3 = r.payload?.id;
await sleep(300);
r = await B.call("plugin.approval.resolve", { id: id3, decision: "deny" });
ok("resolve deny", !!r.ok, S(r.error));
await sleep(700);
const dEv = evs(A, "plugin.approval.resolved", sinceA).find((e) => e.payload?.id === id3);
ok("deny reaches the requester as an event", dEv?.payload?.decision === "deny", dEv ? "" : kinds(A, sinceA));
show("deny event resolvedBy", dEv?.payload?.resolvedBy);

// 8. Unknown id (also what a restarted gateway answers: pending approvals do not survive it).
r = await B.call("plugin.approval.resolve", { id: "does-not-exist", decision: "deny" });
ok("resolve unknown id is refused", !r.ok, S(r.error || r.payload));

A.ws.close(); B.ws.close();
console.log(failures === 0 ? "probe-approval: all checks passed" : `probe-approval: ${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
