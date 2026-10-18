import { useEffect } from "react";
import { listMergeRequests, type MergeRequest, type ToolCall } from "./api";

/** An executed open_merge_request call: the run has opened its request. */
const openedRequest = (call: Pick<ToolCall, "tool_name" | "status" | "is_error">): boolean =>
  call.tool_name.replace(/^yuno__/, "") === "open_merge_request" && call.status === "executed" && !call.is_error;

/**
 * The live fallback for a request opened while the page is in view. The trace carries the run's
 * record when it is loaded, so a page opened on a run that has one needs no lookup; this fetches
 * the record only when an executed open_merge_request call is in view and the run carries no
 * record yet (a run's events do not carry it). Once the record is on the run the effect is idle,
 * and a failed lookup is left to the next load of the run.
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
