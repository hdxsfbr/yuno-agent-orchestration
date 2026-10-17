import type { Project } from "@/lib/api";

/** A project as the API lists it; tokens in tests look like glpat-not-a-real-token-00. */
export const project = (over: Partial<Project> = {}): Project => ({
  id: "p1",
  name: "yuno-playground",
  repo_url: "https://gitlab.com/andrebatista/yuno-playground.git",
  provider: "gitlab",
  default_branch: "main",
  sandbox_network: false,
  description: "",
  token_set: true,
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T09:00:00Z",
  ...over,
});
