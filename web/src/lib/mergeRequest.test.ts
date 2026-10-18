import { describe, expect, it } from "vitest";
import { isStale, mergeBlockedReason, requestNoun, requestRef, threadsText } from "./mergeRequest";

describe("names", () => {
  it("calls a GitLab request a merge request with !, a GitHub one a pull request with #", () => {
    expect(requestNoun("gitlab")).toBe("Merge request");
    expect(requestNoun("github")).toBe("Pull request");
    expect(requestRef({ provider: "gitlab", number: 12 })).toBe("!12");
    expect(requestRef({ provider: "github", number: 3 })).toBe("#3");
  });
});

describe("threadsText", () => {
  it("counts the threads, says none, and says when the provider could not tell", () => {
    expect(threadsText(3)).toBe("3 unresolved threads");
    expect(threadsText(1)).toBe("1 unresolved thread");
    expect(threadsText(0)).toBe("No unresolved threads");
    expect(threadsText(null)).toBe("Threads unknown");
    expect(threadsText(undefined)).toBe("Threads unknown");
  });
});

describe("mergeBlockedReason", () => {
  const mr = (state: "open" | "merged" | "closed", ci: "none" | "pending" | "passed" | "failed", provider: "gitlab" | "github" = "gitlab") => ({
    state,
    ci_status: ci,
    provider,
  });

  it("allows an open request whose pipeline passed or does not exist", () => {
    expect(mergeBlockedReason(mr("open", "passed"))).toBeUndefined();
    expect(mergeBlockedReason(mr("open", "none"))).toBeUndefined();
  });

  it("blocks a request that is not open, whatever its pipeline", () => {
    expect(mergeBlockedReason(mr("merged", "passed"))).toBe("The request is already merged.");
    expect(mergeBlockedReason(mr("closed", "passed"))).toBe("The request is already closed.");
  });

  it("blocks a pipeline that is running or failed, naming the provider to override on", () => {
    expect(mergeBlockedReason(mr("open", "pending"))).toBe("The pipeline is still running.");
    expect(mergeBlockedReason(mr("open", "failed"))).toBe("The pipeline failed. Merge on GitLab to override.");
    expect(mergeBlockedReason(mr("open", "failed", "github"))).toBe("The pipeline failed. Merge on GitHub to override.");
  });
});

describe("isStale", () => {
  const now = Date.parse("2026-10-17T12:00:00Z");
  it("is stale when never synced, unreadable or older than the limit", () => {
    expect(isStale({ synced_at: null }, now, 30_000)).toBe(true);
    expect(isStale({}, now, 30_000)).toBe(true);
    expect(isStale({ synced_at: "never" }, now, 30_000)).toBe(true);
    expect(isStale({ synced_at: "2026-10-17T11:59:29Z" }, now, 30_000)).toBe(true);
  });
  it("is fresh within the limit", () => {
    expect(isStale({ synced_at: "2026-10-17T11:59:30Z" }, now, 30_000)).toBe(false);
    expect(isStale({ synced_at: "2026-10-17T11:59:59Z" }, now, 30_000)).toBe(false);
  });
});
