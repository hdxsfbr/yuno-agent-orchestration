import { useQuery } from "@tanstack/react-query";
import { listProjects } from "./api";

export const projectsKey = ["projects"] as const;

/** The live projects by name. Pass `false` to hold the request back until it is needed. */
export const useProjects = (enabled = true) => useQuery({ queryKey: projectsKey, queryFn: listProjects, enabled });

export const projectFactsKey = (projectId: string) => ["project-facts", projectId] as const;
export const projectMergeRequestsKey = (projectId: string) => ["merge-requests", "project", projectId] as const;
export const projectRunsKey = (projectId: string) => ["runs", "project", projectId] as const;
