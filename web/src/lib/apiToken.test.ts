// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { api, newIdempotencyKey, subscribe } from "./api";
import { clearToken, setToken, signInRequired } from "./auth";

let fake: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  fake = installFakeApi();
  fake.on("GET", "/api/ping", () => ({ ok: true }));
});

afterEach(() => {
  clearToken();
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("api()", () => {
  it("sends the token as a bearer on every call, whatever else the caller sets", async () => {
    setToken("tok-1234");
    fake.on("POST", "/api/things", () => ({}));
    await api("/api/ping");
    await api("/api/things", { method: "POST", body: "{}", headers: { "X-Trace": "t1" } });
    const sent = fake.calls.map((c) => c.headers);
    expect(sent).toHaveLength(2);
    for (const h of sent) expect(h.authorization).toBe("Bearer tok-1234");
    expect(sent[1]["x-trace"]).toBe("t1");
    expect(sent[1]["content-type"]).toBe("application/json");
  });

  it("says every call comes from the UI, and gives every change a fresh Idempotency-Key", async () => {
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) fake.on(method, "/api/things", () => ({}));
    await api("/api/ping");
    for (const method of ["POST", "PUT", "PATCH", "DELETE", "POST"]) await api("/api/things", { method, body: "{}" });
    const sent = fake.calls.map((c) => c.headers);
    for (const h of sent) expect(h["x-yuno-client"]).toBe("web");
    expect(sent[0]["idempotency-key"]).toBeUndefined();
    const keys = sent.slice(1).map((h) => h["idempotency-key"]);
    for (const k of keys) expect(k).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("makes an Idempotency-Key without crypto.randomUUID, which plain HTTP lacks", () => {
    const real = globalThis.crypto;
    vi.stubGlobal("crypto", { getRandomValues: (b: Uint8Array) => real.getRandomValues(b) });
    const a = newIdempotencyKey();
    const b = newIdempotencyKey();
    expect(a).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(a).not.toBe(b);
  });

  it("sends no Authorization header when there is no token", async () => {
    await api("/api/ping");
    expect(fake.calls[0].headers.authorization).toBeUndefined();
  });

  it("never puts the token in the URL", async () => {
    setToken("tok-1234");
    await api("/api/ping");
    expect(fake.calls[0].url).toBe("/api/ping");
  });

  it("fails a refused call with its 401 and asks for sign in", async () => {
    fake.requireToken("right");
    await expect(api("/api/ping")).rejects.toMatchObject({ status: 401, code: "unauthorized" });
    expect(signInRequired()).toBe(true);
  });

  it("does not ask for sign in over an answer to a token that was replaced meanwhile", async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((r) => (release = r));
    fake.on("GET", "/api/slow", async () => {
      await gate;
      return apiError(401, "unauthorized", "missing or invalid bearer token");
    });
    setToken("old");
    const pending = api("/api/slow").catch((e: unknown) => e);
    setToken("new");
    release();
    await pending;
    expect(signInRequired()).toBe(false);
  });
});

describe("subscribe()", () => {
  it("opens the stream with the bearer", async () => {
    setToken("tok-1234");
    const stop = subscribe("/api/events", () => undefined);
    await vi.waitFor(() => expect(fake.streamCalls).toHaveLength(1));
    expect(fake.streamCalls[0].headers.authorization).toBe("Bearer tok-1234");
    expect(fake.streamCalls[0].headers["x-yuno-client"]).toBe("web");
    expect(fake.streamCalls[0].url).toBe("/api/events");
    stop();
  });

  it("reconnects at once with the new token, so a stream never outlives the token it was opened with", async () => {
    setToken("first");
    const stop = subscribe("/api/events", () => undefined);
    await vi.waitFor(() => expect(fake.hasStream("/api/events")).toBe(true));
    setToken("second");
    await vi.waitFor(() => expect(fake.streamCalls.map((c) => c.headers.authorization)).toEqual(["Bearer first", "Bearer second"]), { timeout: 500 });
    stop();
  });

  it("drops the bearer when the operator signs out", async () => {
    setToken("first");
    const stop = subscribe("/api/events", () => undefined);
    await vi.waitFor(() => expect(fake.hasStream("/api/events")).toBe(true));
    clearToken();
    await vi.waitFor(() => expect(fake.streamCalls).toHaveLength(2), { timeout: 500 });
    expect(fake.streamCalls[1].headers.authorization).toBeUndefined();
    stop();
  });

  it("asks for sign in when the stream is refused, and connects as soon as a token is saved", async () => {
    fake.requireToken("right");
    const states: string[] = [];
    const stop = subscribe("/api/events", () => undefined, (s) => states.push(s));
    await vi.waitFor(() => expect(signInRequired()).toBe(true));
    expect(states).not.toContain("open");
    setToken("right");
    await vi.waitFor(() => expect(states).toContain("open"), { timeout: 500 });
    stop();
  });

  it("keeps a healthy stream when the same token is saved again", async () => {
    setToken("same");
    const stop = subscribe("/api/events", () => undefined);
    await vi.waitFor(() => expect(fake.hasStream("/api/events")).toBe(true));
    setToken("same");
    await new Promise((r) => setTimeout(r, 100));
    expect(fake.streamCalls).toHaveLength(1);
    stop();
  });

  it("does not reconnect after it is stopped", async () => {
    const stop = subscribe("/api/events", () => undefined);
    await vi.waitFor(() => expect(fake.streamCalls).toHaveLength(1));
    stop();
    setToken("late");
    await new Promise((r) => setTimeout(r, 50));
    expect(fake.streamCalls).toHaveLength(1);
  });
});
