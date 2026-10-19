import type { ReactNode } from "react";
import type { Approval, ModelCall, RunNode, ToolCall } from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros } from "@/lib/format";
import { isQuestion, questionOutcome } from "@/lib/question";
import { buildTimelineEntries, type RetryMark, type RunView, type TimelineBranch, type TimelineEntry, type TimelineItem, type TimelineNode } from "@/lib/trace";
import { Badge, StatusBadge } from "../Badge";
import { EmptyState } from "../QueryState";
import JsonView from "../JsonView";

type Props = { view: RunView; active: boolean; now: number };

/** Every node visit the entries show, the ones inside a fork's branches included. */
function visitGroups(entry: TimelineEntry): TimelineNode[] {
  switch (entry.kind) {
    case "node":
    case "join":
      return [entry.group];
    case "fork":
      return [entry.group, ...entry.branches.flatMap((b) => b.groups)];
    case "retry":
      return [];
  }
}

export default function Timeline({ view, active, now }: Props) {
  const entries = buildTimelineEntries(view);
  const groups = entries.flatMap(visitGroups);
  // A node that ran more than once (a loop) says which visit each card is.
  const visits = new Map<string, number>();
  for (const g of groups) if (g.node) visits.set(g.node.node_id, (visits.get(g.node.node_id) ?? 0) + 1);
  const repeated = (node: RunNode) => (visits.get(node.node_id) ?? 0) > 1;
  if (groups.length === 0) {
    return <EmptyState>{active ? "Waiting for the first step…" : "This run recorded no steps."}</EmptyState>;
  }
  return (
    <ol className="space-y-3">
      {entries.map((entry) => {
        switch (entry.kind) {
          case "retry":
            return (
              <li key={`retry-${entry.mark.retries}`}>
                <RetryRow mark={entry.mark} />
              </li>
            );
          case "fork":
            return (
              <li key={entry.group.node?.id ?? "fork"}>
                {entry.group.node ? <ForkCard node={entry.group.node} branches={entry.branches} now={now} repeated={repeated} /> : null}
              </li>
            );
          case "join":
            return (
              <li key={entry.group.node?.id ?? "join"}>
                {entry.group.node ? <JoinCard node={entry.group.node} branches={entry.branches} /> : null}
              </li>
            );
          case "node":
            return (
              <li key={entry.group.node?.id ?? "other"}>
                {entry.group.node ? (
                  <NodeCard node={entry.group.node} items={entry.group.items} now={now} repeated={repeated(entry.group.node)} />
                ) : (
                  <OtherActivity items={entry.group.items} />
                )}
              </li>
            );
        }
      })}
    </ol>
  );
}

/** Where the operator retried the run: the failed visit ends above, the new visit of the same step starts below. */
function RetryRow({ mark }: { mark: RetryMark }) {
  return (
    <div className="flex items-center gap-3 text-xs text-zinc-500">
      <span aria-hidden className="h-px flex-1 bg-zinc-300 dark:bg-zinc-700" />
      <span className="font-medium">{`Retry ${mark.retries} from ${mark.from_node}`}</span>
      {mark.at ? <span>{formatDateTime(mark.at)}</span> : null}
      {mark.by ? <span>by {mark.by}</span> : null}
      <span aria-hidden className="h-px flex-1 bg-zinc-300 dark:bg-zinc-700" />
    </div>
  );
}

/** How long a visit took, or has taken so far while it works. */
function visitDuration(node: RunNode, now: number): number | undefined {
  const running = node.status === "running" || node.status === "waiting_approval";
  return node.finished_at ? elapsedMs(node.started_at, node.finished_at) : running ? elapsedMs(node.started_at, undefined, now) : undefined;
}

/** `Started … · Finished … · 3.2s`: the time line of a node card and of a fork card. */
function VisitTimes({ node, now }: { node: RunNode; now: number }) {
  const duration = visitDuration(node, now);
  return (
    <span className="ml-auto text-xs text-zinc-500">
      {node.started_at ? `Started ${formatDateTime(node.started_at)}` : "Not started"}
      {node.finished_at ? ` · Finished ${formatDateTime(node.finished_at)}` : ""}
      {node.started_at && duration !== undefined ? ` · ${formatDuration(duration)}` : ""}
    </span>
  );
}

type CardProps = {
  node: RunNode;
  items: TimelineItem[];
  now: number;
  repeated: boolean;
  /** The head of the branch the visit was made in; the card is then narrow and says which branch it belongs to. */
  branch?: string;
};

function NodeCard({ node, items, now, repeated, branch }: CardProps) {
  const running = node.status === "running" || node.status === "waiting_approval";
  const label = `Node ${node.node_id}${repeated ? `, visit ${node.visit}` : ""}${branch ? `, branch ${branch}` : ""}`;
  return (
    <section className="rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900" aria-label={label}>
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
        <h3 className="font-mono text-sm font-semibold">{node.node_id}</h3>
        <StatusBadge kind="node" status={node.status} />
        {repeated || node.visit > 1 || node.question_id ? (
          // A visit that continues after a question is a second visit of the step because the operator answered.
          <span className="text-xs text-zinc-500">{node.question_id ? `visit ${node.visit}, continued after a question` : `visit ${node.visit}`}</span>
        ) : null}
        {node.iteration > 0 ? (
          <span className="text-xs text-zinc-500">
            {formatCount(node.iteration)} {node.iteration === 1 ? "turn" : "turns"}
          </span>
        ) : null}
        <VisitTimes node={node} now={now} />
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
                <ItemRow item={item} compact={!!branch} />
              </li>
            ))}
          </ul>
        ) : null}
        <JsonView label="Output" value={node.output} defaultOpen />
        {items.length === 0 && !node.failure_reason && (node.output === undefined || node.output === null) ? (
          <p className="text-sm text-zinc-500">{node.status === "waiting_approval" ? "Waiting for an approval." : running ? "Working…" : "No activity recorded."}</p>
        ) : null}
      </div>
    </section>
  );
}

/** Columns of a fork card by branch count: they stack on a narrow screen, and four or five need the wider breakpoint. */
const columns = (n: number) => (n <= 1 ? "" : n === 2 ? "md:grid-cols-2" : n === 3 ? "md:grid-cols-3" : n === 4 ? "lg:grid-cols-4" : "lg:grid-cols-5");

/** A fork visit: the fork's own times, then one column per branch with the node visits made inside it. */
function ForkCard({ node, branches, now, repeated }: { node: RunNode; branches: TimelineBranch[]; now: number; repeated: (n: RunNode) => boolean }) {
  const label = `Fork ${node.node_id}${repeated(node) || node.visit > 1 ? `, visit ${node.visit}` : ""}`;
  return (
    <section className="rounded-lg border border-violet-200 bg-white dark:border-violet-900 dark:bg-zinc-900" aria-label={label}>
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-violet-200 px-4 py-2.5 dark:border-violet-900">
        <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Fork</span>
        <h3 className="font-mono text-sm font-semibold">{node.node_id}</h3>
        <StatusBadge kind="node" status={node.status} />
        {repeated(node) || node.visit > 1 ? <span className="text-xs text-zinc-500">visit {node.visit}</span> : null}
        <VisitTimes node={node} now={now} />
      </header>
      <div className={`grid gap-3 p-4 ${columns(branches.length)}`}>
        {branches.map((b) => (
          <BranchColumn key={b.branch.id} item={b} now={now} repeated={repeated} />
        ))}
      </div>
    </section>
  );
}

function BranchColumn({ item, now, repeated }: { item: TimelineBranch; now: number; repeated: (n: RunNode) => boolean }) {
  const { branch, groups } = item;
  const ended = branch.status !== "running" && branch.status !== "waiting_approval";
  return (
    <section className="min-w-0 space-y-3 rounded-md border border-zinc-200 bg-zinc-50 p-3 dark:border-zinc-800 dark:bg-zinc-950" aria-label={`Branch ${branch.head}`}>
      <header className="flex flex-wrap items-center gap-2">
        <h4 className="font-mono text-sm font-semibold">{branch.head}</h4>
        <StatusBadge kind="branch" status={branch.status} />
      </header>
      {branch.failure_reason ? (
        <p role="alert" className="rounded-md border border-red-200 bg-red-50 p-2.5 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
          {branch.failure_reason}
        </p>
      ) : null}
      {groups.length > 0 ? (
        <ol className="space-y-3">
          {groups.map((g) =>
            g.node ? (
              <li key={g.node.id}>
                <NodeCard node={g.node} items={g.items} now={now} repeated={repeated(g.node)} branch={branch.head} />
              </li>
            ) : null,
          )}
        </ol>
      ) : !branch.failure_reason ? (
        <p className="text-sm text-zinc-500">{ended ? "No activity recorded." : "Starting…"}</p>
      ) : null}
    </section>
  );
}

/** The short card where the branches of a fork visit meet. */
function JoinCard({ node, branches }: { node: RunNode; branches: number }) {
  const at = node.finished_at ?? node.started_at;
  return (
    <section
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-violet-200 bg-white px-4 py-2.5 dark:border-violet-900 dark:bg-zinc-900"
      aria-label={`Join ${node.node_id}`}
    >
      <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Join</span>
      <h3 className="font-mono text-sm font-semibold">{node.node_id}</h3>
      <span className="text-sm">{`Joined ${branches} ${branches === 1 ? "branch" : "branches"}`}</span>
      {at ? <span className="ml-auto text-xs text-zinc-500">{formatDateTime(at)}</span> : null}
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

function ItemRow({ item, compact = false }: { item: TimelineItem; compact?: boolean }) {
  switch (item.kind) {
    case "model_call":
      return <ModelCallRow call={item.call} compact={compact} />;
    case "tool_call":
      return <ToolCallRow call={item.call} compact={compact} />;
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

function ModelCallRow({ call, compact }: { call: ModelCall; compact: boolean }) {
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
      <dl className={`mt-2 grid grid-cols-2 gap-x-6 gap-y-2 ${compact ? "" : "sm:grid-cols-3 lg:grid-cols-6"}`}>
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

function ToolCallRow({ call, compact }: { call: ToolCall; compact: boolean }) {
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
      <div className={`mt-2 grid gap-2 ${compact ? "" : "lg:grid-cols-2"}`}>
        <JsonView label="Input" value={call.input} />
        <JsonView label="Output" value={call.output} />
      </div>
    </Row>
  );
}

function ApprovalRow({ approval }: { approval: Approval }) {
  const outcome = questionOutcome(approval);
  // A declined question carries its reason in the outcome line.
  const reason = isQuestion(approval) && approval.status === "rejected" ? undefined : approval.reason;
  return (
    <Row
      accent="border-l-amber-400 dark:border-l-amber-600"
      title={
        <>
          <span className="text-xs font-medium uppercase tracking-wide text-zinc-500">Approval</span>
          <Badge>{isQuestion(approval) ? "Question" : approval.kind}</Badge>
          <StatusBadge kind="approval" status={approval.status} />
        </>
      }
      right={`Requested ${formatDateTime(approval.requested_at)}${approval.decided_at ? ` · Decided ${formatDateTime(approval.decided_at)}` : ""}`}
    >
      <p className="mt-2 whitespace-pre-wrap break-words text-sm">{approval.message}</p>
      {outcome ? <p className="mt-1 whitespace-pre-wrap break-words text-sm font-medium">{outcome}</p> : null}
      {approval.decided_at && (approval.decided_by || reason) ? (
        <p className="mt-1 text-xs text-zinc-500">
          {approval.decided_by ? `By ${approval.decided_by}` : ""}
          {approval.decided_by && reason ? " · " : ""}
          {reason ?? ""}
        </p>
      ) : null}
      {approval.status === "pending" ? <p className="mt-1 text-xs text-zinc-500">Expires {formatDateTime(approval.expires_at)}</p> : null}
    </Row>
  );
}
