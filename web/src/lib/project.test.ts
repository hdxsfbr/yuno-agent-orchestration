import { describe, expect, it } from "vitest";
import { checkPassed, providerLabel, repoDisplay, toolchainLabel } from "./project";

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
