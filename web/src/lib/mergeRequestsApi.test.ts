// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest, run } from "@/test/fixtures";
import { ApiError, listMergeRequests, mergeMergeRequest, retryRun, syncMergeRequest } from "./api";

let fake: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  fake = installFakeApi();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("merge request endpoints", () => {
  it("lists with the filters that were given, treating null as none", async () => {
    fake.on("GET", /^\/api\/merge-requests/, () => [mergeRequest()]);
    expect((await listMergeRequests()).map((m) => m.number)).toEqual([12]);
    await listMergeRequests({ project_id: "p1", run_id: "r1", state: "open", limit: 5 });
    expect(fake.calls.map((c) => c.url)).toEqual(["/api/merge-requests", "/api/merge-requests?project_id=p1&run_id=r1&state=open&limit=5"]);
    fake.on("GET", /^\/api\/merge-requests/, () => null);
    expect(await listMergeRequests({ project_id: "p1" })).toEqual([]);
  });

  it("syncs and merges with POST and no body, encoding the id", async () => {
    fake.on("POST", "/api/merge-requests/a%2Fb/sync", () => mergeRequest({ id: "a/b", ci_status: "failed" }));
    fake.on("POST", "/api/merge-requests/a%2Fb/merge", () => mergeRequest({ id: "a/b", state: "merged" }));
    expect((await syncMergeRequest("a/b")).ci_status).toBe("failed");
    expect((await mergeMergeRequest("a/b")).state).toBe("merged");
    expect(fake.calls.map((c) => [c.method, c.url, c.body])).toEqual([
      ["POST", "/api/merge-requests/a%2Fb/sync", undefined],
      ["POST", "/api/merge-requests/a%2Fb/merge", undefined],
    ]);
  });

  it("raises the 409 of a merge with the server's code and message", async () => {
    fake.on("POST", "/api/merge-requests/mr1/merge", () => apiError(409, "merge_request_not_mergeable", "Branch cannot be merged"));
    const err = await mergeMergeRequest("mr1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: "merge_request_not_mergeable", message: "Branch cannot be merged" });
  });
});

describe("retryRun", () => {
  it("posts without a body for a plain retry", async () => {
    fake.on("POST", "/api/runs/r1/retry", () => run({ status: "queued", retries: 1 }));
    expect((await retryRun("r1")).retries).toBe(1);
    expect(fake.made("POST", "/api/runs/r1/retry")[0].body).toBeUndefined();
  });

  it("wraps the limits in {limits}", async () => {
    fake.on("POST", "/api/runs/r1/retry", () => run({ status: "queued" }));
    await retryRun("r1", { max_steps: 80, max_cost_usd: 3 });
    expect(fake.made("POST", "/api/runs/r1/retry")[0].body).toEqual({ limits: { max_steps: 80, max_cost_usd: 3 } });
  });

  it("raises the 409 with its code and details", async () => {
    fake.on("POST", "/api/runs/r1/retry", () => apiError(409, "limits_exhausted", "limits are used up", [{ path: "max_steps", message: "40 of 40 used" }]));
    const err = await retryRun("r1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: "limits_exhausted", details: [{ path: "max_steps", message: "40 of 40 used" }] });
  });
});
