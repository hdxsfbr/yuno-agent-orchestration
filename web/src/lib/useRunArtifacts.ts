import { useQuery } from "@tanstack/react-query";
import { listRunArtifacts } from "./api";

export const artifactsKey = (runId: string) => ["run-artifacts", runId] as const;

/** The files the run delivered so far; the run page refreshes it when the run's stream says files were collected. */
export function useRunArtifacts(runId: string) {
  return useQuery({ queryKey: artifactsKey(runId), queryFn: () => listRunArtifacts(runId) });
}
