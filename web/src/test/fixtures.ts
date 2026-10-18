import type { MergeRequest, Project, ProjectFact, ProjectIntake, ProjectSettings, Run, Workflow } from "@/lib/api";

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

/** A run as the API returns it; a failed one by default, which is what the retry tests start from. */
export const run = (over: Partial<Run> = {}): Run => ({
  id: "r1",
  workflow_id: "wf",
  workflow_version: 1,
  trigger: "manual",
  status: "failed",
  input: {},
  state: {},
  limits: {},
  current_node: "coder",
  step_count: 3,
  tool_call_count: 5,
  cost_usd_micros: 120_000,
  created_at: "2026-10-15T10:00:00Z",
  started_at: "2026-10-15T10:00:01Z",
  finished_at: "2026-10-15T10:05:00Z",
  ...over,
});

/** A merge request record: open, with its pipeline passed, on GitLab. */
export const mergeRequest = (over: Partial<MergeRequest> = {}): MergeRequest => ({
  id: "mr1",
  project_id: "p1",
  run_id: "r1",
  provider: "gitlab",
  number: 12,
  url: "https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12",
  source_branch: "yuno/0123456789ab",
  target_branch: "main",
  title: "Add the health endpoint",
  state: "open",
  ci_status: "passed",
  unresolved_threads: 0,
  synced_at: new Date().toISOString(),
  created_at: "2026-10-15T10:01:00Z",
  updated_at: "2026-10-15T10:02:00Z",
  ...over,
});

/** A note an agent saved for a project. */
export const projectFact = (over: Partial<ProjectFact> = {}): ProjectFact => ({
  id: "f1",
  project_id: "p1",
  key: "test-command",
  value: "go test ./...",
  agent_id: "a1",
  agent_name: "Coder",
  source_run_id: "r1",
  created_at: "2026-10-15T10:03:00Z",
  updated_at: "2026-10-15T10:03:00Z",
  ...over,
});

/** The snapshot of a project a run keeps: what the project's settings were when the run was queued. */
export const projectSettings = (over: Partial<ProjectSettings> = {}): ProjectSettings => ({
  name: "yuno-playground",
  repo_url: "https://gitlab.com/andrebatista/yuno-playground.git",
  provider: "gitlab",
  default_branch: "main",
  sandbox_network: false,
  ...over,
});

/** A project's issue intake: on, with the default label and a workflow, never polled. */
export const intake = (over: Partial<ProjectIntake> = {}): ProjectIntake => ({
  enabled: true,
  label: "yuno",
  workflow_id: "w1",
  workflow_name: "Dev pipeline (ours)",
  ...over,
});

/** An operator's workflow that works on a project and takes a task. */
export const workflow = (over: Partial<Workflow> = {}): Workflow => ({
  id: "w1",
  name: "Dev pipeline (ours)",
  description: "",
  version: 1,
  definition: { nodes: [], edges: [], project: true, input_schema: { type: "object", properties: { task: { type: "string" } }, required: ["task"] } },
  created_at: "2026-10-15T09:00:00Z",
  updated_at: "2026-10-15T09:00:00Z",
  ...over,
});
