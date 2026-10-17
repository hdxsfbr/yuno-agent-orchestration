import type { ReactNode } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router";
import { cancelRun, delegatedConversationId, isDelegatedRun, isTerminalStatus, mergeRequestOf, type Run, type ToolCall } from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros, shortId } from "@/lib/format";
import type { Connection } from "@/lib/useEventStream";
import Button from "../Button";
import { Card } from "../Card";
import ConnectionIndicator from "../ConnectionIndicator";
import { Badge, RunStatusBadge } from "../Badge";
import { InlineError, errorText } from "../QueryState";

function Item({ label, children, title }: { label: string; children: ReactNode; title?: string }) {
  return (
    <div title={title}>
      <dt className="text-xs uppercase tracking-wide text-zinc-500">{label}</dt>
      <dd className="mt-0.5 text-sm font-medium tabular-nums">{children}</dd>
    </div>
  );
}

/** The Trigger value: where a delegated run came from, or the trigger and its reference. */
function TriggerValue({ run }: { run: Run }) {
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
  return (
    <>
      {run.trigger}
      {run.trigger_ref ? <span className="ml-1.5 font-mono text-xs font-normal text-zinc-500">{run.trigger_ref}</span> : null}
    </>
  );
}

type Props = {
  run: Run;
  connection: Connection;
  now: number;
  /** The run's tool calls so far: the merge request link comes from them. */
  toolCalls?: readonly ToolCall[];
  /** Called with the run the API returns after a cancel request. */
  onRunChange: (run: Run) => void;
};

const TOOLCHAIN_LABEL: Record<string, string> = { go: "Go", node: "Node", python: "Python", default: "default image" };

export default function RunHeader({ run, connection, now, toolCalls = [], onRunChange }: Props) {
  const cancel = useMutation({ mutationFn: () => cancelRun(run.id), onSuccess: onRunChange });
  const terminal = isTerminalStatus(run.status);
  const requested = cancel.isSuccess || !!run.cancel_requested;
  const duration = elapsedMs(run.started_at, run.finished_at, now);
  const mergeRequest = mergeRequestOf(toolCalls);
  const requestLabel = mergeRequest?.provider === "github" ? "Pull request" : "Merge request";

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
                  Toolchain: {TOOLCHAIN_LABEL[run.toolchain] ?? run.toolchain}
                </Badge>
              ) : null}
              {mergeRequest ? (
                <a href={mergeRequest.url} target="_blank" rel="noreferrer" className="text-sm font-medium underline">
                  {requestLabel}
                  {mergeRequest.number !== undefined ? ` ${mergeRequest.provider === "github" ? "#" : "!"}${mergeRequest.number}` : ""}
                </a>
              ) : null}
            </div>
            <p className="mt-1 break-all font-mono text-xs text-zinc-500">{run.id}</p>
          </div>
          <div className="flex items-center gap-3">
            <ConnectionIndicator state={connection} />
            {terminal ? null : (
              <Button variant="danger" onClick={() => cancel.mutate()} disabled={requested || cancel.isPending}>
                {cancel.isPending ? "Cancelling…" : requested ? "Cancel requested" : "Cancel run"}
              </Button>
            )}
          </div>
        </div>

        {cancel.error ? (
          <div className="mt-3">
            <InlineError>Could not cancel the run: {errorText(cancel.error)}</InlineError>
          </div>
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
          {run.current_node && !terminal ? (
            <Item label="Current node">
              <span className="font-mono">{run.current_node}</span>
            </Item>
          ) : null}
          <Item label="Created">{formatDateTime(run.created_at)}</Item>
          <Item label="Started">{formatDateTime(run.started_at)}</Item>
          <Item label="Finished">{formatDateTime(run.finished_at)}</Item>
          <Item label="Duration" title={run.paused_total_ms ? `Includes ${formatDuration(run.paused_total_ms)} waiting for approval` : undefined}>
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
    </section>
  );
}
