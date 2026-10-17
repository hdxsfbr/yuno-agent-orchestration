// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { installFakeApi } from "@/test/fakeApi";
import { project } from "@/test/fixtures";
import { checkProject, createProject, deleteProject, getProject, listProjects, updateProject, type ProjectInput } from "./api";

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
