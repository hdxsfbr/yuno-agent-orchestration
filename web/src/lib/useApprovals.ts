import { useCallback } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listApprovals, type Approval, type ApprovalItem, type ApprovalStatus, type SSEEvent } from "./api";
import { useEventStream, type Connection } from "./useEventStream";

/** One cache entry per status filter; "all" is the unfiltered list. */
export const approvalsKey = (status?: ApprovalStatus) => ["approvals", status ?? "all"] as const;

export function useApprovalList(status?: ApprovalStatus) {
  return useQuery({ queryKey: approvalsKey(status), queryFn: () => listApprovals({ status }) });
}

/** Requests waiting for a decision; the sidebar badge and the inbox share this entry. */
export const usePendingApprovals = () => useApprovalList("pending");

/** Applies a decision the API returned to every cached list: it leaves the pending one at once. */
export function useApprovalCache() {
  const qc = useQueryClient();
  return useCallback(
    (approval: Pick<Approval, "id">) => {
      qc.setQueryData<ApprovalItem[]>(approvalsKey("pending"), (old) => old?.filter((a) => a.id !== approval.id));
      void qc.invalidateQueries({ queryKey: ["approvals"] });
    },
    [qc],
  );
}

/**
 * Keeps the approval lists current from the global event stream: a new request refreshes
 * them, a decision (by anyone, from any surface) takes the row out of the pending list.
 */
export function useApprovalEvents(): Connection {
  const qc = useQueryClient();
  const decided = useApprovalCache();
  const onEvent = useCallback(
    (e: SSEEvent) => {
      if (e.event === "approval.requested") {
        void qc.invalidateQueries({ queryKey: ["approvals"] });
      } else if (e.event === "approval.decided") {
        const a = (e.data as { approval?: Approval } | null)?.approval;
        if (a?.id) decided(a);
        else void qc.invalidateQueries({ queryKey: ["approvals"] });
      }
    },
    [qc, decided],
  );
  return useEventStream("/api/events", onEvent);
}
