// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { TOKEN_KEY, authHeaders, clearToken, getToken, reportUnauthorized, setToken, signInRequired, subscribeAuth } from "./auth";

afterEach(() => {
  vi.restoreAllMocks();
  clearToken();
  localStorage.clear();
});

describe("token store", () => {
  it("keeps the token in localStorage under yuno.apiToken, trimmed", () => {
    setToken("  tok-1234  ");
    expect(TOKEN_KEY).toBe("yuno.apiToken");
    expect(localStorage.getItem("yuno.apiToken")).toBe("tok-1234");
    expect(getToken()).toBe("tok-1234");
  });

  it("reads a token stored by an earlier visit", () => {
    localStorage.setItem("yuno.apiToken", "from-before");
    expect(getToken()).toBe("from-before");
  });

  it("forgets the token on clear", () => {
    setToken("tok-1234");
    clearToken();
    expect(localStorage.getItem("yuno.apiToken")).toBeNull();
    expect(getToken()).toBe("");
  });

  it("holds the token for the tab when the browser refuses storage", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    setToken("tok-1234");
    expect(getToken()).toBe("tok-1234");
  });

  it("tells listeners when the token changes", () => {
    const seen = vi.fn();
    const off = subscribeAuth(seen);
    setToken("tok-1234");
    clearToken();
    expect(seen).toHaveBeenCalledTimes(2);
    off();
    setToken("tok-5678");
    expect(seen).toHaveBeenCalledTimes(2);
  });
});

describe("other tabs", () => {
  it("tells listeners when another tab signs in or out, and ignores unrelated storage changes", () => {
    const seen = vi.fn();
    const off = subscribeAuth(seen);
    window.dispatchEvent(new StorageEvent("storage", { key: "yuno.apiToken" }));
    window.dispatchEvent(new StorageEvent("storage", { key: "some.other.key" }));
    expect(seen).toHaveBeenCalledTimes(1);
    off();
  });
});

describe("authHeaders", () => {
  it("is a bearer for the token in use, and nothing without one", () => {
    expect(authHeaders()).toEqual({});
    setToken("tok-1234");
    expect(authHeaders()).toEqual({ Authorization: "Bearer tok-1234" });
    expect(authHeaders("other")).toEqual({ Authorization: "Bearer other" });
  });
});

describe("reportUnauthorized", () => {
  it("asks for sign in when the token that was refused is the one in use", () => {
    setToken("tok-1234");
    expect(signInRequired()).toBe(false);
    reportUnauthorized("tok-1234");
    expect(signInRequired()).toBe(true);
  });

  it("ignores a refusal of a token that has since been replaced", () => {
    setToken("new-token");
    reportUnauthorized("old-token");
    expect(signInRequired()).toBe(false);
  });

  it("asks again after a new token is saved and refused", () => {
    setToken("wrong");
    reportUnauthorized("wrong");
    setToken("still-wrong");
    expect(signInRequired()).toBe(false);
    reportUnauthorized("still-wrong");
    expect(signInRequired()).toBe(true);
  });
});
