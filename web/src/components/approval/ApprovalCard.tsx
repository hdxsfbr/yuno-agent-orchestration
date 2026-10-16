import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router";
import { ApiError, approveApproval, rejectApproval, type Approval, type ToolCall } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { Badge, StatusBadge } from "../Badge";
import Button from "../Button";
import JsonView from "../JsonView";
import { InlineError, errorText } from "../QueryState";
import { control } from "../fields";

const KIND_LABEL: Record<Approval["kind"], string> = {
  exec: "Command",
  tool: "Tool call",
  node: "Workflow step",
};

type Props = {
  approval: Approval;
  /** The tool call a tool or command approval is about, when known. */
  toolCall?: ToolCall;
  /** Show the run as a link (the inbox) rather than leaving it implied (the run page). */
  showRun?: boolean;
  /** Called with the approval the API returned after a decision. */
  onDecided?: (approval: Approval) => void;
  /** Called after a decision attempt that changed nothing here, such as a 409. */
  onStale?: () => void;
};

/** One request for a decision: what it is about, who it concerns, and the buttons to answer it. */
export default function ApprovalCard({ approval, toolCall, showRun = false, onDecided, onStale }: Props) {
  const [reason, setReason] = useState("");
  const [alreadyDecided, setAlreadyDecided] = useState(false);

  const decide = useMutation({
    mutationFn: (choice: "approve" | "approve_always" | "reject") => {
      const why = reason.trim();
      const body = why ? { reason: why } : {};
      if (choice === "reject") return rejectApproval(approval.id, body);
      return approveApproval(approval.id, choice === "approve_always" ? { ...body, always: true } : body);
    },
    onSuccess: (decided) => {
      setAlreadyDecided(false);
      onDecided?.(decided);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        setAlreadyDecided(true);
        onStale?.();
      }
    },
  });

  const pending = approval.status === "pending" && !alreadyDecided;
  const failed = decide.error && !(decide.error instanceof ApiError && decide.error.status === 409);

  return (
    <article aria-label={`Approval ${shortId(approval.id)}`} className="rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <Badge tone="zinc" title={approval.kind}>
          {KIND_LABEL[approval.kind] ?? approval.kind}
        </Badge>
        <StatusBadge kind="approval" status={approval.status} />
        {toolCall ? <span className="font-mono text-sm font-medium">{toolCall.tool_name}</span> : null}
        <span className="ml-auto text-xs text-zinc-500">Requested {formatDateTime(approval.requested_at)}</span>
      </header>

      <dl className="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-xs text-zinc-500">
        {showRun ? (
          <div className="flex gap-1.5">
            <dt>{approval.run_id ? "Run" : "Raised in"}</dt>
            <dd>
              {approval.run_id ? (
                <Link to={`/runs/${approval.run_id}`} className="font-mono text-zinc-900 hover:underline dark:text-zinc-100" title={approval.run_id}>
                  {shortId(approval.run_id)}
                </Link>
              ) : (
                <Link to="/channels" className="text-zinc-900 hover:underline dark:text-zinc-100">
                  Conversation
                </Link>
              )}
            </dd>
          </div>
        ) : null}
        <div className="flex gap-1.5">
          <dt>Node</dt>
          <dd className="font-mono text-zinc-900 dark:text-zinc-100">{approval.node_id || "—"}</dd>
        </div>
        {approval.status === "pending" || approval.status === "expired" ? (
          <div className="flex gap-1.5">
            <dt>{approval.status === "pending" ? "Expires" : "Expired"}</dt>
            <dd className="text-zinc-900 dark:text-zinc-100">{formatDateTime(approval.expires_at)}</dd>
          </div>
        ) : null}
      </dl>

      {approval.message ? <p className="mt-3 whitespace-pre-wrap break-words text-sm">{approval.message}</p> : null}
      {toolCall ? (
        <div className="mt-3">
          <JsonView label={`Input to ${toolCall.tool_name}`} value={toolCall.input} defaultOpen />
        </div>
      ) : null}

      {approval.status !== "pending" ? (
        <p className="mt-3 text-xs text-zinc-500">
          {approval.decided_at ? `Decided ${formatDateTime(approval.decided_at)}` : "Not decided"}
          {approval.decided_by ? ` by ${approval.decided_by}` : ""}
          {approval.reason ? ` · ${approval.reason}` : ""}
        </p>
      ) : null}

      {alreadyDecided ? (
        <p role="alert" className="mt-3 text-sm text-amber-700 dark:text-amber-400">
          This approval was already decided.
        </p>
      ) : null}
      {failed ? (
        <div className="mt-3">
          <InlineError>Could not record the decision: {errorText(decide.error)}</InlineError>
        </div>
      ) : null}

      {pending ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <input
            type="text"
            aria-label="Reason"
            placeholder="Reason (optional)"
            value={reason}
            maxLength={500}
            onChange={(e) => setReason(e.target.value)}
            className={control(false, "!w-64 max-w-full")}
          />
          <Button variant="primary" onClick={() => decide.mutate("approve")} disabled={decide.isPending}>
            Approve
          </Button>
          {approval.kind === "exec" ? (
            <Button onClick={() => decide.mutate("approve_always")} disabled={decide.isPending} title="Approve this command and allow it from now on">
              Approve always
            </Button>
          ) : null}
          <Button variant="danger" onClick={() => decide.mutate("reject")} disabled={decide.isPending}>
            Reject
          </Button>
        </div>
      ) : null}
    </article>
  );
}
