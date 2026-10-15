import type { ReactNode } from "react";
import { useMutation } from "@tanstack/react-query";
import { cancelRun, isTerminalStatus, type Run } from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros, shortId } from "@/lib/format";
import type { Connection } from "@/lib/useEventStream";
import Button from "../Button";
import { Card } from "../Card";
import ConnectionIndicator from "../ConnectionIndicator";
import { RunStatusBadge } from "../Badge";
import { InlineError, errorText } from "../QueryState";

function Item({ label, children, title }: { label: string; children: ReactNode; title?: string }) {
  return (
    <div title={title}>
      <dt className="text-xs uppercase tracking-wide text-zinc-500">{label}</dt>
      <dd className="mt-0.5 text-sm font-medium tabular-nums">{children}</dd>
    </div>
  );
}

type Props = {
  run: Run;
  connection: Connection;
  now: number;
  /** Called with the run the API returns after a cancel request. */
  onRunChange: (run: Run) => void;
};

export default function RunHeader({ run, connection, now, onRunChange }: Props) {
  const cancel = useMutation({ mutationFn: () => cancelRun(run.id), onSuccess: onRunChange });
  const terminal = isTerminalStatus(run.status);
  const requested = cancel.isSuccess || !!run.cancel_requested;
  const duration = elapsedMs(run.started_at, run.finished_at, now);

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
            {run.trigger}
            {run.trigger_ref ? <span className="ml-1.5 font-mono text-xs font-normal text-zinc-500">{run.trigger_ref}</span> : null}
          </Item>
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
