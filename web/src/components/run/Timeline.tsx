import type { ReactNode } from "react";
import type { Approval, ModelCall, RunNode, ToolCall } from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros } from "@/lib/format";
import { buildTimeline, type RunView, type TimelineItem } from "@/lib/trace";
import { Badge, StatusBadge } from "../Badge";
import { EmptyState } from "../QueryState";
import JsonView from "../JsonView";

type Props = { view: RunView; active: boolean; now: number };

export default function Timeline({ view, active, now }: Props) {
  const groups = buildTimeline(view);
  if (groups.length === 0) {
    return <EmptyState>{active ? "Waiting for the first step…" : "This run recorded no steps."}</EmptyState>;
  }
  return (
    <ol className="space-y-3">
      {groups.map((g) => (
        <li key={g.node?.id ?? "other"}>{g.node ? <NodeCard node={g.node} items={g.items} now={now} /> : <OtherActivity items={g.items} />}</li>
      ))}
    </ol>
  );
}

function NodeCard({ node, items, now }: { node: RunNode; items: TimelineItem[]; now: number }) {
  const running = node.status === "running" || node.status === "waiting_approval";
  const duration = node.finished_at ? elapsedMs(node.started_at, node.finished_at) : running ? elapsedMs(node.started_at, undefined, now) : undefined;
  return (
    <section className="rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900" aria-label={`Node ${node.node_id}`}>
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
        <h3 className="font-mono text-sm font-semibold">{node.node_id}</h3>
        <StatusBadge kind="node" status={node.status} />
        {node.visit > 1 ? <span className="text-xs text-zinc-500">visit {node.visit}</span> : null}
        {node.iteration > 0 ? (
          <span className="text-xs text-zinc-500">
            {formatCount(node.iteration)} {node.iteration === 1 ? "turn" : "turns"}
          </span>
        ) : null}
        <span className="ml-auto text-xs text-zinc-500">
          {node.started_at ? `Started ${formatDateTime(node.started_at)}` : "Not started"}
          {node.finished_at ? ` · Finished ${formatDateTime(node.finished_at)}` : ""}
          {node.started_at && duration !== undefined ? ` · ${formatDuration(duration)}` : ""}
        </span>
      </header>
      <div className="space-y-3 p-4">
        {node.failure_reason ? (
          <p role="alert" className="rounded-md border border-red-200 bg-red-50 p-2.5 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
            {node.failure_reason}
          </p>
        ) : null}
        {items.length > 0 ? (
          <ul className="space-y-2">
            {items.map((item) => (
              <li key={item.key}>
                <ItemRow item={item} />
              </li>
            ))}
          </ul>
        ) : null}
        <JsonView label="Output" value={node.output} defaultOpen />
        {items.length === 0 && !node.failure_reason && (node.output === undefined || node.output === null) ? (
          <p className="text-sm text-zinc-500">{running ? "Working…" : "No activity recorded."}</p>
        ) : null}
      </div>
    </section>
  );
}

function OtherActivity({ items }: { items: TimelineItem[] }) {
  return (
    <section className="rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900" aria-label="Other activity">
      <header className="border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
        <h3 className="text-sm font-semibold">Other activity</h3>
      </header>
      <ul className="space-y-2 p-4">
        {items.map((item) => (
          <li key={item.key}>
            <ItemRow item={item} />
          </li>
        ))}
      </ul>
    </section>
  );
}

function ItemRow({ item }: { item: TimelineItem }) {
  switch (item.kind) {
    case "model_call":
      return <ModelCallRow call={item.call} />;
    case "tool_call":
      return <ToolCallRow call={item.call} />;
    case "approval":
      return <ApprovalRow approval={item.approval} />;
  }
}

function Row({ accent, title, right, children }: { accent: string; title: ReactNode; right?: ReactNode; children?: ReactNode }) {
  return (
    <div className={`rounded-md border border-zinc-200 border-l-4 ${accent} p-3 dark:border-zinc-800`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <div className="flex flex-wrap items-center gap-2 text-sm">{title}</div>
        {right ? <div className="ml-auto text-xs text-zinc-500">{right}</div> : null}
      </div>
      {children}
    </div>
  );
}

function Metric({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div>
      <dt className="text-xs text-zinc-500">{label}</dt>
      <dd className="text-sm tabular-nums">{value}</dd>
    </div>
  );
}

function ModelCallRow({ call }: { call: ModelCall }) {
  return (
    <Row
      accent="border-l-violet-400 dark:border-l-violet-600"
      title={
        <>
          <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Model call</span>
          <span className="font-mono font-medium">{call.model}</span>
          {call.attempt > 1 ? <Badge tone="amber">attempt {call.attempt}</Badge> : null}
        </>
      }
      right={formatDateTime(call.started_at)}
    >
      <dl className="mt-2 grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-3 lg:grid-cols-6">
        <Metric label="Tokens in" value={formatCount(call.input_tokens)} />
        <Metric label="Tokens out" value={formatCount(call.output_tokens)} />
        <Metric label="Cache read / write" value={`${formatCount(call.cache_read_tokens)} / ${formatCount(call.cache_write_tokens)}`} />
        <Metric label="Cost" value={formatMicros(call.cost_usd_micros)} />
        <Metric label="Stop reason" value={call.stop_reason || "—"} />
        <Metric label="Duration" value={formatDuration(call.duration_ms)} />
      </dl>
      {call.error ? (
        <p role="alert" className="mt-2 text-sm text-red-700 dark:text-red-400">
          {call.error}
        </p>
      ) : null}
    </Row>
  );
}

function ToolCallRow({ call }: { call: ToolCall }) {
  return (
    <Row
      accent="border-l-sky-400 dark:border-l-sky-600"
      title={
        <>
          <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Tool call</span>
          <span className="font-mono font-medium">{call.tool_name}</span>
          <StatusBadge kind="tool" status={call.status} />
          {call.is_error && call.status !== "failed" ? <Badge tone="red">error result</Badge> : null}
          {call.attempt > 1 ? <Badge tone="amber">attempt {call.attempt}</Badge> : null}
        </>
      }
      right={`${call.started_at ? formatDateTime(call.started_at) + " · " : ""}${formatDuration(call.duration_ms)}`}
    >
      <div className="mt-2 grid gap-2 lg:grid-cols-2">
        <JsonView label="Input" value={call.input} />
        <JsonView label="Output" value={call.output} />
      </div>
    </Row>
  );
}

function ApprovalRow({ approval }: { approval: Approval }) {
  return (
    <Row
      accent="border-l-amber-400 dark:border-l-amber-600"
      title={
        <>
          <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Approval</span>
          <Badge>{approval.kind}</Badge>
          <StatusBadge kind="approval" status={approval.status} />
        </>
      }
      right={`Requested ${formatDateTime(approval.requested_at)}${approval.decided_at ? ` · Decided ${formatDateTime(approval.decided_at)}` : ""}`}
    >
      <p className="mt-2 whitespace-pre-wrap break-words text-sm">{approval.message}</p>
      {approval.decided_at && (approval.decided_by || approval.reason) ? (
        <p className="mt-1 text-xs text-zinc-500">
          {approval.decided_by ? `By ${approval.decided_by}` : ""}
          {approval.decided_by && approval.reason ? " · " : ""}
          {approval.reason ?? ""}
        </p>
      ) : null}
      {approval.status === "pending" ? <p className="mt-1 text-xs text-zinc-500">Expires {formatDateTime(approval.expires_at)}</p> : null}
    </Row>
  );
}
