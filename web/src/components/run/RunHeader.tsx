import { useState, type ReactNode } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router";
import {
  ApiError,
  cancelRun,
  delegatedConversationId,
  isDelegatedRun,
  isTerminalStatus,
  issueRef,
  pauseRun,
  resumeRun,
  retryRun,
  steerRun,
  type Approval,
  type Run,
  type RunBranch,
  type ToolCall,
} from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros, shortId } from "@/lib/format";
import { requestNoun, requestRef } from "@/lib/mergeRequest";
import { shipRunRef } from "@/lib/plan";
import { pendingQuestion, questionLine } from "@/lib/question";
import { toolchainLabel } from "@/lib/project";
import { canRetry, repeatsSideEffects } from "@/lib/runRetry";
import { forkProgress } from "@/lib/trace";
import type { Connection } from "@/lib/useEventStream";
import Button from "../Button";
import { Card } from "../Card";
import { control } from "../fields";
import ConnectionIndicator from "../ConnectionIndicator";
import { Badge, RunStatusBadge } from "../Badge";
import PlanWaveLink from "../project/PlanWaveLink";
import ProjectLink, { hasProject } from "../project/ProjectLink";
import { InlineError, errorText } from "../QueryState";
import RetryDialog from "./RetryDialog";

function Item({ label, children, title }: { label: string; children: ReactNode; title?: string }) {
  return (
    <div title={title}>
      <dt className="text-xs uppercase tracking-wide text-zinc-500">{label}</dt>
      <dd className="mt-0.5 text-sm font-medium tabular-nums">{children}</dd>
    </div>
  );
}

/** The Trigger value: where a delegated run came from, the issue an issue run came from, or the trigger and its reference. */
function TriggerValue({ run }: { run: Run }) {
  const issue = issueRef(run);
  if (issue) {
    return issue.url ? (
      <a href={issue.url} target="_blank" rel="noreferrer" className="underline" title="Open the issue on the provider">
        issue #{issue.number}
      </a>
    ) : (
      <>issue #{issue.number}</>
    );
  }
  if (isDelegatedRun(run)) {
    const conversation = delegatedConversationId(run);
    return conversation ? (
      <Link to={`/channels/${encodeURIComponent(conversation)}`} className="underline">
        Delegated from a conversation
      </Link>
    ) : (
      <>Delegated</>
    );
  }
  if (shipRunRef(run)) return <PlanWaveLink run={run} />;
  return (
    <>
      {run.trigger}
      {run.trigger_ref ? <span className="ml-1.5 font-mono text-xs font-normal text-zinc-500">{run.trigger_ref}</span> : null}
    </>
  );
}

/** The request a run updates, as a link to the provider next to the project: "Updates merge request !12", "Updates pull request #12" on GitHub. */
function UpdatedRequest({ run }: { run: Run }) {
  const mr = run.merge_request;
  if (!run.merge_request_id || !mr || mr.id !== run.merge_request_id) return null;
  return (
    <a href={mr.url} target="_blank" rel="noreferrer" className="ml-3 whitespace-nowrap font-normal underline" title={mr.title}>
      Updates {requestNoun(mr.provider).toLowerCase()} {requestRef(mr)}
    </a>
  );
}

/** `2 branches running`, `1 waiting for an answer`, or both joined by a comma: how the branches of the fork stand. */
function forkCounts({ running, waiting }: { running: number; waiting: number }): string {
  const parts: string[] = [];
  if (running > 0) parts.push(`${running} ${running === 1 ? "branch" : "branches"} running`);
  if (waiting > 0) parts.push(`${waiting} waiting for an answer`);
  return parts.join(", ");
}

type Props = {
  run: Run;
  connection: Connection;
  now: number;
  /** The run's tool calls so far: the retry dialog says when a retry would repeat what some of them did. */
  toolCalls?: readonly ToolCall[];
  /** The run's approvals so far: while the run waits on a question, the header says what it asks. */
  approvals?: readonly Approval[];
  /** The run's branches so far: while the run is inside a fork, the step line says how many of them are running and how many wait for an answer. */
  branches?: readonly RunBranch[];
  /** Called with the run the API returns after a cancel or a retry. */
  onRunChange: (run: Run) => void;
};

export default function RunHeader({ run, connection, now, toolCalls = [], approvals = [], branches = [], onRunChange }: Props) {
  const cancel = useMutation({ mutationFn: () => cancelRun(run.id), onSuccess: onRunChange });
  // A refusal for a run that used up a limit opens the dialog; any other refusal is told inline.
  const [limitsRefusal, setLimitsRefusal] = useState<ApiError | null>(null);
  // A retried run starts a new live period: the cancel the last one asked for must not keep Cancel disabled.
  const retried = (next: Run) => {
    cancel.reset();
    onRunChange(next);
  };
  const retry = useMutation({
    mutationFn: () => retryRun(run.id),
    onSuccess: retried,
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409 && err.code === "limits_exhausted") setLimitsRefusal(err);
    },
  });
  const pause = useMutation({ mutationFn: () => pauseRun(run.id), onSuccess: onRunChange });
  const [note, setNote] = useState("");
  const resume = useMutation({
    mutationFn: () => resumeRun(run.id, note),
    onSuccess: (next: Run) => {
      setNote("");
      pause.reset();
      onRunChange(next);
    },
  });
  const [steering, setSteering] = useState(false);
  const [message, setMessage] = useState("");
  const steer = useMutation({
    mutationFn: () => steerRun(run.id, message.trim()),
    onSuccess: (next: Run) => {
      setMessage("");
      setSteering(false);
      onRunChange(next);
    },
  });
  const terminal = isTerminalStatus(run.status);
  const requested = cancel.isSuccess || !!run.cancel_requested;
  const paused = run.status === "paused";
  const live = run.status === "running";
  const duration = elapsedMs(run.started_at, run.finished_at, now);
  const retries = run.retries ?? 0;
  const retryError = retry.error && !limitsRefusal ? retry.error : null;
  const asking = run.status === "waiting_approval" ? pendingQuestion(approvals) : undefined;
  // Inside a fork the run's step is the fork until the join passes; the line says how many branches still work.
  const inFork = terminal ? undefined : forkProgress(branches, run.current_node);
  const step = inFork && inFork.running + inFork.waiting > 0 ? `${inFork.fork} (${forkCounts(inFork)})` : run.current_node;

  return (
    <section aria-label="Run summary">
      <Card>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-xl font-semibold">
                Run <span className="font-mono">{shortId(run.id)}</span>
              </h1>
              <RunStatusBadge status={run.status} />
              {isDelegatedRun(run) ? <Badge tone="blue">Delegated</Badge> : null}
              {run.toolchain ? (
                <Badge tone="zinc" title="The toolchain read from the project checkout; the sandbox image follows it">
                  Toolchain: {toolchainLabel(run.toolchain)}
                </Badge>
              ) : null}
              {retries > 0 ? (
                <Badge tone="zinc" title="How many times the run was retried from the step it stopped at">
                  Retried {retries}×
                </Badge>
              ) : null}
            </div>
            <p className="mt-1 break-all font-mono text-xs text-zinc-500">{run.id}</p>
          </div>
          <div className="flex items-center gap-3">
            <ConnectionIndicator state={connection} />
            {terminal ? (
              canRetry(run) ? (
                <Button variant="primary" onClick={() => retry.mutate()} disabled={retry.isPending} title="Run again from the step it stopped at">
                  {retry.isPending ? "Retrying…" : "Retry"}
                </Button>
              ) : null
            ) : (
              <>
                {paused ? (
                  <>
                    <input
                      aria-label="Note for the step"
                      placeholder="Note for the step (optional)"
                      value={note}
                      maxLength={2000}
                      onChange={(e) => setNote(e.target.value)}
                      className={control(false, "!w-56")}
                    />
                    <Button variant="primary" onClick={() => resume.mutate()} disabled={resume.isPending || requested} title="Continue the step it was paused in">
                      {resume.isPending ? "Resuming…" : "Resume"}
                    </Button>
                  </>
                ) : !requested && (live || run.status === "queued") ? (
                  <Button onClick={() => pause.mutate()} disabled={pause.isPending} title="Stop the step's turn and hold the run until it is resumed">
                    {pause.isPending ? "Pausing…" : "Pause"}
                  </Button>
                ) : null}
                {live && !requested ? (
                  <Button onClick={() => setSteering((open) => !open)} aria-expanded={steering} title="Send a message into the running step">
                    Steer
                  </Button>
                ) : null}
                <Button variant="danger" onClick={() => cancel.mutate()} disabled={requested || cancel.isPending}>
                  {cancel.isPending ? "Cancelling…" : requested ? "Cancel requested" : "Cancel run"}
                </Button>
              </>
            )}
          </div>
        </div>

        {steering && live && !requested ? (
          <form
            className="mt-3 flex flex-col gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              steer.mutate();
            }}
          >
            <label htmlFor="steer-message" className="text-sm font-medium">
              Message to the running step
            </label>
            <textarea id="steer-message" rows={3} maxLength={4000} value={message} onChange={(e) => setMessage(e.target.value)} className={control()} />
            <div className="flex gap-2">
              <Button type="submit" variant="primary" disabled={!message.trim() || steer.isPending}>
                {steer.isPending ? "Sending…" : "Send"}
              </Button>
              <Button onClick={() => setSteering(false)}>Close</Button>
            </div>
          </form>
        ) : null}
        {steer.isSuccess && live ? <p className="mt-3 text-sm text-zinc-600 dark:text-zinc-400">Sent to the running step.</p> : null}
        {pause.isSuccess && live ? <p className="mt-3 text-sm text-zinc-600 dark:text-zinc-400">Pause requested: the run pauses once its step stops.</p> : null}

        {cancel.error ? (
          <div className="mt-3">
            <InlineError>Could not cancel the run: {errorText(cancel.error)}</InlineError>
          </div>
        ) : null}
        {pause.error ? (
          <div className="mt-3">
            <InlineError>Could not pause the run: {errorText(pause.error)}</InlineError>
          </div>
        ) : null}
        {resume.error ? (
          <div className="mt-3">
            <InlineError>Could not resume the run: {errorText(resume.error)}</InlineError>
          </div>
        ) : null}
        {steer.error ? (
          <div className="mt-3">
            <InlineError>Could not send the message: {errorText(steer.error)}</InlineError>
          </div>
        ) : null}
        {retryError ? (
          <div className="mt-3">
            <InlineError>Could not retry the run: {errorText(retryError)}</InlineError>
          </div>
        ) : null}
        {asking ? (
          <p className="mt-3 text-sm">
            <a href="#approvals" className="font-medium underline">
              Asks: {questionLine(asking.message)}
            </a>
          </p>
        ) : null}
        {run.failure_reason ? (
          <p role="alert" className="mt-3 rounded-md border border-red-200 bg-red-50 p-2.5 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
            {run.failure_reason}
          </p>
        ) : null}

        <dl className="mt-4 grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-6">
          <Item label="Trigger">
            <TriggerValue run={run} />
          </Item>
          {hasProject(run) ? (
            <Item label="Project">
              <ProjectLink run={run} className="underline" />
              <UpdatedRequest run={run} />
            </Item>
          ) : null}
          {run.current_node && !terminal ? (
            <Item label="Current node">
              <span className="font-mono">{step}</span>
            </Item>
          ) : null}
          <Item label="Created">{formatDateTime(run.created_at)}</Item>
          <Item label="Started">{formatDateTime(run.started_at)}</Item>
          <Item label="Finished">{formatDateTime(run.finished_at)}</Item>
          <Item label="Duration" title={run.paused_total_ms ? `Includes ${formatDuration(run.paused_total_ms)} waiting for approval or paused` : undefined}>
            {formatDuration(duration)}
          </Item>
          <Item label="Steps">{formatCount(run.step_count)}</Item>
          <Item label="Tool calls">{formatCount(run.tool_call_count)}</Item>
          <Item label="Tokens in">{formatCount(run.input_tokens)}</Item>
          <Item label="Cache read / write">
            {formatCount(run.cache_read_tokens ?? 0)} / {formatCount(run.cache_write_tokens ?? 0)}
          </Item>
          <Item label="Tokens out">{formatCount(run.output_tokens)}</Item>
          <Item label="Cost">{formatMicros(run.cost_usd_micros)}</Item>
        </dl>
      </Card>
      {limitsRefusal ? (
        <RetryDialog
          run={run}
          refusal={limitsRefusal}
          repeatsSideEffects={repeatsSideEffects(toolCalls)}
          onClose={() => {
            retry.reset();
            setLimitsRefusal(null);
          }}
          onRetried={retried}
        />
      ) : null}
    </section>
  );
}
