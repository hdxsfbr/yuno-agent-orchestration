// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { intake, project, projectFact } from "@/test/fixtures";
import { ApiError, checkProject, createProject, deleteProject, deleteProjectFact, getProject, listProjectFacts, listProjects, listRuns, pollIntake, updateProject, type ProjectInput } from "./api";

let fake: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  fake = installFakeApi();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("project endpoints", () => {
  it("lists the projects, treating null as none", async () => {
    fake.on("GET", "/api/projects", () => [project({})]);
    expect((await listProjects()).map((p) => p.name)).toEqual(["yuno-playground"]);
    fake.on("GET", "/api/projects", () => null);
    expect(await listProjects()).toEqual([]);
  });

  it("gets one project by id, encoding the id", async () => {
    fake.on("GET", "/api/projects/a%2Fb", () => project({ id: "a/b" }));
    expect((await getProject("a/b")).id).toBe("a/b");
  });

  it("creates with POST and the token in the body", async () => {
    fake.on("POST", "/api/projects", () => project({}));
    const input: ProjectInput = { name: "api", repo_url: "https://gitlab.com/a/b", provider: "", token: "glpat-not-a-real-token-00" };
    await createProject(input);
    expect(fake.made("POST", "/api/projects")[0].body).toEqual(input);
  });

  it("updates with PUT, deletes with DELETE and checks with POST", async () => {
    fake.on("PUT", "/api/projects/p1", () => project({}));
    fake.on("DELETE", "/api/projects/p1", () => new Response(null, { status: 204 }));
    fake.on("POST", "/api/projects/p1/check", () => ({ ok: true, message: "Token accepted: default branch main, push allowed.", checked_at: "2026-10-16T10:00:00Z" }));

    await updateProject("p1", { name: "api", repo_url: "https://gitlab.com/a/b" });
    expect(fake.made("PUT", "/api/projects/p1")[0].body).toEqual({ name: "api", repo_url: "https://gitlab.com/a/b" });
    expect(await deleteProject("p1")).toBeUndefined();
    expect(fake.made("DELETE", "/api/projects/p1")).toHaveLength(1);
    expect((await checkProject("p1")).ok).toBe(true);
    expect(fake.made("POST", "/api/projects/p1/check")[0].body).toBeUndefined();
  });
});

describe("project settings of wave 8", () => {
  it("sends the instructions and both caps, null clearing a cap", async () => {
    fake.on("PUT", "/api/projects/p1", () => project({}));
    await updateProject("p1", { name: "api", repo_url: "https://gitlab.com/a/b", instructions: "Use make.", max_cost_usd_per_day: 5, max_cost_usd_per_month: null });
    expect(fake.made("PUT", "/api/projects/p1")[0].body).toEqual({
      name: "api",
      repo_url: "https://gitlab.com/a/b",
      instructions: "Use make.",
      max_cost_usd_per_day: 5,
      max_cost_usd_per_month: null,
    });
  });

  it("returns the instructions, the caps and the spend as the API sent them", async () => {
    fake.on("GET", "/api/projects", () => [
      project({ instructions: "Use make.", max_cost_usd_per_day: 5, spend: { day_usd: 1.2, month_usd: 12, day_until: "2026-10-18T00:00:00Z", month_until: "2026-11-01T00:00:00Z" } }),
    ]);
    const [p] = await listProjects();
    expect(p).toMatchObject({ instructions: "Use make.", max_cost_usd_per_day: 5, spend: { day_usd: 1.2, month_usd: 12 } });
    expect(p.max_cost_usd_per_month).toBeUndefined();
  });
});

describe("project notes endpoints", () => {
  it("lists the notes, treating null as none, and encodes the ids", async () => {
    fake.on("GET", "/api/projects/a%2Fb/facts", () => [projectFact({ project_id: "a/b" })]);
    expect((await listProjectFacts("a/b")).map((f) => f.key)).toEqual(["test-command"]);
    fake.on("GET", "/api/projects/p1/facts", () => null);
    expect(await listProjectFacts("p1")).toEqual([]);
  });

  it("deletes one note with DELETE", async () => {
    fake.on("DELETE", "/api/projects/p1/facts/f%2F1", () => new Response(null, { status: 204 }));
    expect(await deleteProjectFact("p1", "f/1")).toBeUndefined();
    expect(fake.made("DELETE", "/api/projects/p1/facts/f%2F1")).toHaveLength(1);
  });
});

describe("listing a project's runs", () => {
  it("names the project and the limit in the query, and treats null as none", async () => {
    fake.on("GET", /^\/api\/runs\?/, () => null);
    expect(await listRuns({ project_id: "p 1", limit: 10 })).toEqual([]);
    expect(fake.made("GET", /^\/api\/runs\?/).map((c) => c.url)).toEqual(["/api/runs?project_id=p+1&limit=10"]);
  });

  it("sends no project_id when none is asked for", async () => {
    fake.on("GET", /^\/api\/runs\?/, () => []);
    await listRuns({ status: "failed" });
    expect(fake.made("GET", /^\/api\/runs\?/).map((c) => c.url)).toEqual(["/api/runs?status=failed&limit=100"]);
  });
});

describe("listing runs by trigger and merge request", () => {
  it("names the trigger and the merge request record in the query, between the project and the limit", async () => {
    fake.on("GET", /^\/api\/runs\?/, () => []);
    await listRuns({ status: "failed", project_id: "p1", trigger: "issue", merge_request_id: "mr 1", limit: 200 });
    await listRuns({ trigger: "schedule" });
    await listRuns({ merge_request_id: "mr1" });
    expect(fake.made("GET", /^\/api\/runs\?/).map((c) => c.url)).toEqual([
      "/api/runs?status=failed&project_id=p1&trigger=issue&merge_request_id=mr+1&limit=200",
      "/api/runs?trigger=schedule&limit=100",
      "/api/runs?merge_request_id=mr1&limit=100",
    ]);
  });
});

describe("issue intake", () => {
  it("sends the intake settings with the project and returns them with what the last poll did", async () => {
    const settings = { enabled: true, label: "yuno", workflow_id: "w1" };
    fake.on("PUT", "/api/projects/p1", (r) => project({ intake: intake({ polled_at: "2026-10-18T09:00:00Z", last_error: "intake: no workflow", ...(r.body as ProjectInput).intake }) }));
    const saved = await updateProject("p1", { name: "n", repo_url: "https://gitlab.com/g/p", intake: settings });
    expect(fake.made("PUT", "/api/projects/p1")[0].body).toMatchObject({ intake: settings });
    expect(saved.intake).toMatchObject({ enabled: true, label: "yuno", workflow_name: "Dev pipeline (ours)", polled_at: "2026-10-18T09:00:00Z", last_error: "intake: no workflow" });
  });

  it("leaves intake out of the body when the form did not send it", async () => {
    fake.on("PUT", "/api/projects/p1", () => project());
    await updateProject("p1", { name: "n", repo_url: "https://gitlab.com/g/p" });
    expect(fake.made("PUT", "/api/projects/p1")[0].body).not.toHaveProperty("intake");
  });

  it("polls with POST and no body, encoding the id, and returns what the pass did", async () => {
    fake.on("POST", "/api/projects/a%2Fb/intake/poll", () => ({ taken: 2, closed: 1 }));
    expect(await pollIntake("a/b")).toEqual({ taken: 2, closed: 1 });
    expect(fake.made("POST", "/api/projects/a%2Fb/intake/poll")[0].body).toBeUndefined();
  });

  it("raises the 409 of a project whose intake is off with the server's code and message", async () => {
    fake.on("POST", "/api/projects/p1/intake/poll", () => apiError(409, "intake_disabled", "issue intake is off for this project"));
    const err = await pollIntake("p1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: "intake_disabled", message: "issue intake is off for this project" });
  });
});
