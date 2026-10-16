// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { api, subscribe } from "./api";
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
