// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { installFakeApi } from "@/test/fakeApi";
import { project, projectFact } from "@/test/fixtures";
import { checkProject, createProject, deleteProject, deleteProjectFact, getProject, listProjectFacts, listProjects, listRuns, updateProject, type ProjectInput } from "./api";

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
