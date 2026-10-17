import { useQuery } from "@tanstack/react-query";
import { listProjects } from "./api";

export const projectsKey = ["projects"] as const;

/** The live projects by name. Pass `false` to hold the request back until it is needed. */
export const useProjects = (enabled = true) => useQuery({ queryKey: projectsKey, queryFn: listProjects, enabled });
