import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router";
import { ApiError, answerApproval, approveApproval, rejectApproval, type Approval, type ToolCall } from "@/lib/api";
import { errorsFor, parseFieldErrors } from "@/lib/agentForm";
import { formatDateTime, shortId } from "@/lib/format";
import { questionOutcome } from "@/lib/question";
import { Badge, StatusBadge } from "../Badge";
import Button from "../Button";
import JsonView from "../JsonView";
import { InlineError, errorText } from "../QueryState";
import { FieldErrors, control } from "../fields";

const KIND_LABEL: Record<Approval["kind"], string> = {
  exec: "Command",
  tool: "Tool call",
  node: "Workflow step",
  question: "Question",
};

/** The most an answer can be (DESIGN 5.24 rule 3); the server counts bytes and has the last word. */
const MAX_ANSWER = 4096;

/** What a 422 says is wrong with an answer: the details on `answer`, else every detail with the field it names. */
function answerProblems(details: unknown): string[] {
  const all = parseFieldErrors(details);
  const own = errorsFor(all, "answer");
  if (own.length > 0) return own;
  return Object.entries(all).flatMap(([path, messages]) => messages.map((m) => (path ? `${path}: ${m}` : m)));
}

type Props = {
  approval: Approval;
  /** The tool call a tool or command approval is about, when known. */
  toolCall?: ToolCall;
  /** Show the run as a link (the inbox) rather than leaving it implied (the run page). */
  showRun?: boolean;
  /** The approval's run has already ended: a tool call still open then answers a late grant. */
  runEnded?: boolean;
  /** Called with the approval the API returned after a decision. */
  onDecided?: (approval: Approval) => void;
  /** Called after a decision attempt that changed nothing here, such as a 409. */
  onStale?: () => void;
};

/** One request for a decision: what it is about, who it concerns, and the buttons to answer it. */
export default function ApprovalCard({ approval, toolCall, showRun = false, runEnded = false, onDecided, onStale }: Props) {
  const [reason, setReason] = useState("");
  const [alreadyDecided, setAlreadyDecided] = useState(false);
  // What the server said is wrong with the answer, as it came back on a 422.
  const [answerErrors, setAnswerErrors] = useState<string[]>([]);

  const decide = useMutation({
    mutationFn: (choice: { answer: string } | "approve" | "approve_always" | "reject") => {
      if (typeof choice === "object") return answerApproval(approval.id, choice);
      const why = reason.trim();
      const body = why ? { reason: why } : {};
      if (choice === "reject") return rejectApproval(approval.id, body);
      return approveApproval(approval.id, choice === "approve_always" ? { ...body, always: true } : body);
    },
    onMutate: () => setAnswerErrors([]),
    onSuccess: (record) => {
      setAlreadyDecided(false);
      onDecided?.(record);
    },
    onError: (err, choice) => {
      if (!(err instanceof ApiError)) return;
      if (err.status === 409) {
        setAlreadyDecided(true);
        onStale?.();
      } else if (err.status === 422 && typeof choice === "object") {
        setAnswerErrors(answerProblems(err.details));
      }
    },
  });

  const question = approval.kind === "question";
  const pending = approval.status === "pending" && !alreadyDecided;
  const busy = decide.isPending;
  const outcome = questionOutcome(approval);
  // A 409 is told as "already decided" and a 422 on the answer next to the box; anything else is told here.
  const failure = decide.error;
  const failed = !!failure && !(failure instanceof ApiError && (failure.status === 409 || (failure.status === 422 && answerErrors.length > 0)));

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

      {outcome ? <p className="mt-3 whitespace-pre-wrap break-words text-sm font-medium">{outcome}</p> : null}
      {approval.status !== "pending" ? (
        <p className="mt-3 text-xs text-zinc-500">
          {approval.decided_at ? `Decided ${formatDateTime(approval.decided_at)}` : "Not decided"}
          {approval.decided_by ? ` by ${approval.decided_by}` : ""}
          {/* A declined question has its reason in the line above. */}
          {approval.reason && !(question && approval.status === "rejected") ? ` · ${approval.reason}` : ""}
        </p>
      ) : null}

      {alreadyDecided ? (
        <p role="alert" className="mt-3 text-sm text-amber-700 dark:text-amber-400">
          This approval was already decided.
        </p>
      ) : null}
      {failed ? (
        <div className="mt-3">
          <InlineError>Could not record the decision: {errorText(failure)}</InlineError>
        </div>
      ) : null}

      {pending && runEnded && approval.kind === "tool" ? (
        <p role="note" className="mt-3 text-sm text-amber-700 dark:text-amber-400">
          This run has already ended. Approving still executes the call; rejecting discards it.
        </p>
      ) : null}

      {pending && question ? (
        <QuestionControls
          options={approval.options ?? []}
          busy={busy}
          errors={answerErrors}
          reason={reason}
          onReason={setReason}
          onAnswer={(text) => decide.mutate({ answer: text })}
          onDecline={() => decide.mutate("reject")}
        />
      ) : null}

      {pending && !question ? (
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
          <Button variant="primary" onClick={() => decide.mutate("approve")} disabled={busy}>
            Approve
          </Button>
          {approval.kind === "exec" ? (
            <Button onClick={() => decide.mutate("approve_always")} disabled={busy} title="Approve this command and allow it from now on">
              Approve always
            </Button>
          ) : null}
          <Button variant="danger" onClick={() => decide.mutate("reject")} disabled={busy}>
            Reject
          </Button>
        </div>
      ) : null}
    </article>
  );
}

type QuestionControlsProps = {
  options: readonly string[];
  busy: boolean;
  /** What the server said is wrong with the answer. */
  errors: string[];
  reason: string;
  onReason: (reason: string) => void;
  onAnswer: (answer: string) => void;
  onDecline: () => void;
};

/**
 * How the operator deals with a question: one button for each answer the agent suggested, a box for any
 * other answer, and Decline with an optional reason. Approve is not offered: a question needs an answer.
 */
function QuestionControls({ options, busy, errors, reason, onReason, onAnswer, onDecline }: QuestionControlsProps) {
  const [text, setText] = useState("");
  const typed = text.trim();
  return (
    <div className="mt-3 space-y-3">
      {options.length > 0 ? (
        <div role="group" aria-label="Suggested answers" className="flex flex-wrap gap-2">
          {options.map((option) => (
            <Button key={option} className="max-w-full text-left" onClick={() => onAnswer(option)} disabled={busy}>
              {option}
            </Button>
          ))}
        </div>
      ) : null}
      <div>
        <div className="flex flex-wrap items-start gap-2">
          <textarea
            aria-label="Your answer"
            placeholder={options.length > 0 ? "Or write your own answer" : "Your answer"}
            rows={2}
            value={text}
            maxLength={MAX_ANSWER}
            aria-invalid={errors.length > 0 ? true : undefined}
            onChange={(e) => setText(e.target.value)}
            className={control(errors.length > 0, "min-w-64 flex-1")}
          />
          <Button variant="primary" onClick={() => onAnswer(typed)} disabled={busy || typed === ""}>
            Answer
          </Button>
        </div>
        <FieldErrors errors={errors} />
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="text"
          aria-label="Reason"
          placeholder="Reason (optional)"
          value={reason}
          maxLength={500}
          onChange={(e) => onReason(e.target.value)}
          className={control(false, "!w-64 max-w-full")}
        />
        <Button variant="danger" onClick={onDecline} disabled={busy}>
          Decline
        </Button>
      </div>
    </div>
  );
}
