import { describe, expect, it } from "vitest";
import { BUDGET_TEXT, budgetTone, capText, checkPassed, parseCap, providerLabel, repoDisplay, toolchainLabel } from "./project";

describe("repoDisplay", () => {
  it("shows the host and the path", () => {
    expect(repoDisplay("https://gitlab.com/group/project")).toBe("gitlab.com/group/project");
    expect(repoDisplay("https://github.com/acme/docs.git")).toBe("github.com/acme/docs.git");
    expect(repoDisplay("https://git.example.com:8443/a/b/c/")).toBe("git.example.com:8443/a/b/c");
  });

  it("shows what is not a URL as it is", () => {
    expect(repoDisplay("not a url")).toBe("not a url");
    expect(repoDisplay("")).toBe("");
  });
});

describe("labels", () => {
  it("names the providers and the toolchains", () => {
    expect(providerLabel("gitlab")).toBe("GitLab");
    expect(providerLabel("github")).toBe("GitHub");
    expect(providerLabel("gitea")).toBe("gitea");
    expect(toolchainLabel("go")).toBe("Go");
    expect(toolchainLabel("node")).toBe("Node");
    expect(toolchainLabel("python")).toBe("Python");
    expect(toolchainLabel("default")).toBe("default image");
    expect(toolchainLabel("rust")).toBe("rust");
  });
});

describe("checkPassed", () => {
  it("needs the token accepted, and nothing the answer reports against push rights or the branch", () => {
    expect(checkPassed({ ok: true, can_push: true, branch_exists: true })).toBe(true);
    expect(checkPassed({ ok: true })).toBe(true);
    expect(checkPassed({ ok: false })).toBe(false);
    expect(checkPassed({ ok: true, can_push: false })).toBe(false);
    expect(checkPassed({ ok: true, branch_exists: false })).toBe(false);
    expect(checkPassed({ ok: false, can_push: true, branch_exists: true })).toBe(false);
  });
});

describe("budgetTone", () => {
  it("has no tone without a cap", () => {
    expect(budgetTone(3, undefined)).toBe("none");
    expect(budgetTone(3, null)).toBe("none");
  });

  it("is ok below 80 %, a warning from 80 % and danger at the cap or over it", () => {
    expect(budgetTone(0, 10)).toBe("ok");
    expect(budgetTone(7.999999, 10)).toBe("ok");
    expect(budgetTone(8, 10)).toBe("warning");
    expect(budgetTone(9.999999, 10)).toBe("warning");
    expect(budgetTone(10, 10)).toBe("danger");
    expect(budgetTone(14, 10)).toBe("danger");
  });

  it("compares whole micro-dollars, so float sums do not tip a boundary", () => {
    expect(budgetTone(0.1 + 0.2, 0.3)).toBe("danger");
    expect(budgetTone(0.8, 1)).toBe("warning");
    expect(budgetTone(0.1 * 3 * 2 + 0.2, 1)).toBe("warning");
  });

  it("counts a missing spend as nothing", () => {
    expect(budgetTone(undefined, 5)).toBe("ok");
    expect(BUDGET_TEXT.none).toContain("zinc");
    expect(BUDGET_TEXT.warning).toContain("amber");
    expect(BUDGET_TEXT.danger).toContain("red");
  });
});

describe("parseCap", () => {
  it("reads an empty field as no cap", () => {
    expect(parseCap("")).toEqual({ ok: true, value: null });
    expect(parseCap("   ")).toEqual({ ok: true, value: null });
  });

  it("reads a positive amount", () => {
    expect(parseCap("5")).toEqual({ ok: true, value: 5 });
    expect(parseCap(" 12.50 ")).toEqual({ ok: true, value: 12.5 });
    expect(parseCap(".5")).toEqual({ ok: true, value: 0.5 });
    expect(parseCap("5.")).toEqual({ ok: true, value: 5 });
    expect(parseCap("1000000")).toEqual({ ok: true, value: 1_000_000 });
  });

  it("refuses what is not an amount", () => {
    for (const text of ["abc", "$5", "1e3", "1,5", "5 USD", "NaN", "Infinity"]) {
      const r = parseCap(text);
      expect(r.ok).toBe(false);
    }
  });

  it("refuses zero and below", () => {
    expect(parseCap("0")).toEqual({ ok: false, message: "Must be greater than 0" });
    expect(parseCap("0.00")).toEqual({ ok: false, message: "Must be greater than 0" });
    expect(parseCap("-3")).toEqual({ ok: false, message: "Must be greater than 0" });
  });
});

describe("capText", () => {
  it("is empty for no cap", () => {
    expect(capText(undefined)).toBe("");
    expect(capText(null)).toBe("");
    expect(capText(5)).toBe("5");
    expect(capText(12.5)).toBe("12.5");
  });
});
