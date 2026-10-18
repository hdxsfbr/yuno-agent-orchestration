import { useEffect } from "react";
import { listMergeRequests, type MergeRequest, type ToolCall } from "./api";

/** An executed open_merge_request call: the run has opened its request. */
const openedRequest = (call: Pick<ToolCall, "tool_name" | "status" | "is_error">): boolean =>
  call.tool_name.replace(/^yuno__/, "") === "open_merge_request" && call.status === "executed" && !call.is_error;

/**
 * Fetches the run's merge request record when its trace shows the request was just opened but the
 * run in view does not carry the record yet (the run is joined with it when it is read, not when
 * its events arrive). A failed lookup is left to the next load of the run.
 */
export function useOpenedMergeRequest(runId: string | undefined, record: MergeRequest | undefined, toolCalls: readonly ToolCall[] | undefined, apply: (mr: MergeRequest) => void) {
  const opened = !record && !!toolCalls?.some(openedRequest);
  useEffect(() => {
    if (!opened || !runId) return;
    let cancelled = false;
    listMergeRequests({ run_id: runId, limit: 1 }).then(
      (found) => {
        if (!cancelled && found[0]) apply(found[0]);
      },
      () => undefined,
    );
    return () => {
      cancelled = true;
    };
  }, [opened, runId, apply]);
}
