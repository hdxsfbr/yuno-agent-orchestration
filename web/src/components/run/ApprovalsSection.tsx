import { isTerminalStatus, type Approval, type RunStatus, type ToolCall } from "@/lib/api";
import ApprovalCard from "../approval/ApprovalCard";

type Props = {
  approvals: Approval[];
  toolCalls: ToolCall[];
  runStatus: RunStatus;
  /** Called with the approval the API returned after a decision, so the page updates at once. */
  onDecided: (approval: Approval) => void;
};

/** Pending requests first (oldest first), then the decided ones in the order they were asked. */
function ordered(approvals: Approval[]): Approval[] {
  const at = (a: Approval) => Date.parse(a.requested_at) || 0;
  const pending = approvals.filter((a) => a.status === "pending").sort((a, b) => at(a) - at(b));
  const rest = approvals.filter((a) => a.status !== "pending").sort((a, b) => at(a) - at(b));
  return [...pending, ...rest];
}

/** The run's approval requests with the buttons to answer them where they are still open. */
export default function ApprovalsSection({ approvals, toolCalls, runStatus, onDecided }: Props) {
  const pending = approvals.filter((a) => a.status === "pending");
  return (
    <div className="space-y-3">
      {runStatus === "waiting_approval" ? (
        <p role="status" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
          {pending.length > 0
            ? `This run is paused until ${pending.length === 1 ? "this request is" : `these ${pending.length} requests are`} decided${pending.length === 1 && pending[0].node_id ? ` (node ${pending[0].node_id})` : ""}.`
            : "This run is paused, waiting for an approval."}
        </p>
      ) : null}
      <ul className="space-y-3">
        {ordered(approvals).map((a) => (
          <li key={a.id}>
            <ApprovalCard
              approval={a}
              toolCall={a.tool_call_id ? toolCalls.find((c) => c.id === a.tool_call_id) : undefined}
              runEnded={isTerminalStatus(runStatus)}
              onDecided={onDecided}
            />
          </li>
        ))}
      </ul>
    </div>
  );
}
