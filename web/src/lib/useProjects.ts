import { useQuery } from "@tanstack/react-query";
import { listProjects, listRuns } from "./api";

export const projectsKey = ["projects"] as const;

/** The live projects by name. Pass `false` to hold the request back until it is needed. */
export const useProjects = (enabled = true) => useQuery({ queryKey: projectsKey, queryFn: listProjects, enabled });

export const projectFactsKey = (projectId: string) => ["project-facts", projectId] as const;
export const projectMergeRequestsKey = (projectId: string) => ["merge-requests", "project", projectId] as const;
/** The open merge requests of one project: what the Merge request selectors offer. */
export const openMergeRequestsKey = (projectId: string) => ["merge-requests", "open", projectId] as const;
export const projectRunsKey = (projectId: string) => ["runs", "project", projectId] as const;

/** How many of a project's newest runs the project page reads. */
export const PROJECT_RUNS_LIMIT = 200;

/**
 * The project's newest runs, newest first, read once for the whole project page: the Recent runs card shows the top of
 * the list and the Merge requests table counts, per request, the runs that updated it.
 */
export const useProjectRuns = (projectId: string) =>
  useQuery({ queryKey: projectRunsKey(projectId), queryFn: () => listRuns({ project_id: projectId, limit: PROJECT_RUNS_LIMIT }) });
