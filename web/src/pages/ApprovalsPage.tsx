import { useSearchParams } from "react-router";
import { isTerminalStatus, type ApprovalItem, type ApprovalStatus } from "@/lib/api";
import { humanize } from "@/lib/format";
import { useApprovalCache, useApprovalEvents, useApprovalList } from "@/lib/useApprovals";
import ApprovalCard from "@/components/approval/ApprovalCard";
import { PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { control } from "@/components/fields";
import { EmptyState, Loading, QueryError } from "@/components/QueryState";

const STATUSES: ApprovalStatus[] = ["pending", "approved", "rejected", "expired"];
const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function parseFilter(value: string | null): ApprovalStatus | "all" {
  if (value === "all") return "all";
  return STATUSES.find((s) => s === value) ?? "pending";
}

/** Waiting requests first in the order they arrived; decided ones newest first. */
function sorted(items: ApprovalItem[], filter: ApprovalStatus | "all"): ApprovalItem[] {
  const at = (a: ApprovalItem) => Date.parse(a.requested_at) || 0;
  return items.slice().sort((a, b) => (filter === "pending" ? at(a) - at(b) : at(b) - at(a)));
}

export default function ApprovalsPage() {
  const [params, setParams] = useSearchParams();
  const filter = parseFilter(params.get("status"));
  const q = useApprovalList(filter === "all" ? undefined : filter);
  const connection = useApprovalEvents();
  const decided = useApprovalCache();

  return (
    <div className="max-w-4xl">
      <PageHeader title="Approvals">
        <ConnectionIndicator state={connection} />
        <label className="flex items-center gap-2 text-sm">
          <span className="text-zinc-500">Show</span>
          <select
            value={filter}
            onChange={(e) => {
              const next = new URLSearchParams(params);
              if (e.target.value === "pending") next.delete("status");
              else next.set("status", e.target.value);
              setParams(next, { replace: true });
            }}
            className={control(false, "!w-auto")}
          >
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {capitalize(humanize(s))}
              </option>
            ))}
            <option value="all">All</option>
          </select>
        </label>
      </PageHeader>

      {q.isLoading ? <Loading label="Loading approvals…" /> : null}
      {q.error ? <QueryError what="approvals" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? (
        <EmptyState>{filter === "pending" ? "Nothing is waiting for approval." : filter === "all" ? "No approvals yet." : `No ${humanize(filter)} approvals.`}</EmptyState>
      ) : null}
      {q.data && q.data.length > 0 ? (
        <ul className="space-y-3">
          {sorted(q.data, filter).map((a) => (
            <li key={a.id}>
              <ApprovalCard approval={a} toolCall={a.tool_call} showRun runEnded={!!a.run_status && isTerminalStatus(a.run_status)} onDecided={decided} />
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
