// Self-test of the one security-relevant behaviour of the recorder: the api token is attached to the app's
// own /api/ requests, and to nothing else (other paths, other origins, the page itself). It starts two
// throwaway local servers and needs no stack. Run it in the browser container:
//   docker compose --profile tools run --rm -T --no-deps --entrypoint node demo /demo/selftest-auth.mjs
import http from "node:http";
import { createRequire } from "node:module";
import path from "node:path";

const FAKE = "selftest-token-not-a-credential";
const seen = { api: [], page: [], other: [] };
const listen = (handler) => new Promise((resolve) => { const s = http.createServer(handler); s.listen(0, "127.0.0.1", () => resolve(s)); });

const app = await listen((req, res) => {
  if (req.url.startsWith("/api/")) {
    seen.api.push(req.headers.authorization ?? "");
    res.setHeader("content-type", "application/json");
    res.end('{"ok":true}');
  } else {
    seen.page.push(req.headers.authorization ?? "");
    res.setHeader("content-type", "text/html");
    res.end("<!doctype html><title>app</title><body>app</body>");
  }
});
const other = await listen((req, res) => {
  seen.other.push(req.headers.authorization ?? "");
  res.setHeader("access-control-allow-origin", "*");
  res.end("x");
});
const base = `http://127.0.0.1:${app.address().port}`;
const foreign = `http://127.0.0.1:${other.address().port}`;

process.env.API_TOKEN = FAKE;
process.env.BASE_URL = base;
const { installAuth, HERE } = await import("./helpers.mjs");
const { chromium } = createRequire(path.join(process.env.PW_DIR || path.join(HERE, ".pw"), "/"))("playwright-core");

const browser = await chromium.launch();
const context = await browser.newContext();
await installAuth(context);
const page = await context.newPage();
await page.goto(`${base}/`);
await page.evaluate(async (foreignBase) => {
  await fetch("/api/ping");
  await fetch("/not-the-api.txt");
  await fetch(`${foreignBase}/api/ping`, { mode: "no-cors" });
}, foreign);
const leaked = await page.evaluate((t) => document.documentElement.outerHTML.includes(t) || JSON.stringify(localStorage).includes(t), FAKE);
await browser.close();
app.close();
other.close();

const checks = [
  ["api request carries the bearer token", seen.api.length === 1 && seen.api[0] === `Bearer ${FAKE}`],
  ["page and non-api requests carry nothing", seen.page.every((h) => h === "")],
  ["another origin receives nothing", seen.other.length > 0 && seen.other.every((h) => h === "")],
  ["the token is not in the page", !leaked],
];
for (const [what, ok] of checks) console.log(`${ok ? "PASS" : "FAIL"}  ${what}`);
process.exit(checks.every(([, ok]) => ok) ? 0 : 1);
