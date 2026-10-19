import type { ReactNode } from "react";
import { Link, useParams } from "react-router";
import { isTerminalStatus } from "@/lib/api";
import { useNow } from "@/lib/useNow";
import { useOpenedMergeRequest } from "@/lib/useOpenedMergeRequest";
import { useRunTrace } from "@/lib/useRunTrace";
import { Card } from "@/components/Card";
import { Loading, QueryError } from "@/components/QueryState";
import ApprovalsSection from "@/components/run/ApprovalsSection";
import ArtifactsSection from "@/components/run/ArtifactsSection";
import LogSection from "@/components/run/LogSection";
import MergeRequestCard from "@/components/run/MergeRequestCard";
import MessagesSection from "@/components/run/MessagesSection";
import RunHeader from "@/components/run/RunHeader";
import StateSection from "@/components/run/StateSection";
import Timeline from "@/components/run/Timeline";

function Section({ title, count, id, children }: { title: string; count?: number; id?: string; children: ReactNode }) {
  return (
    <section id={id}>
      <h2 className="mb-2 text-base font-semibold">
        {title}
        {count !== undefined ? <span className="ml-2 text-sm font-normal text-zinc-500">{count}</span> : null}
      </h2>
      {children}
    </section>
  );
}

export default function RunDetailPage() {
  const { id = "" } = useParams();
  const { view, error, connection, retry, applyRun, applyApproval, applyMergeRequest } = useRunTrace(id);
  useOpenedMergeRequest(view?.run.id, view?.run.merge_request, view?.tool_calls, applyMergeRequest);
  const active = view ? !isTerminalStatus(view.run.status) : false;
  const now = useNow(active);

  const back = (
    <Link to="/runs" className="text-sm text-zinc-500 hover:underline">
      ← Runs
    </Link>
  );

  if (!view) {
    return (
      <div className="space-y-3">
        {back}
        {error ? <QueryError what="the run" error={error} onRetry={retry} /> : <Loading label="Loading run…" />}
      </div>
    );
  }

  return (
    <div className="max-w-5xl space-y-5">
      {back}
      <RunHeader run={view.run} connection={connection} now={now} toolCalls={view.tool_calls} approvals={view.approvals} branches={view.branches} onRunChange={applyRun} />
      {view.run.merge_request ? <MergeRequestCard key={view.run.merge_request.id} mr={view.run.merge_request} run={view.run} onChange={applyMergeRequest} /> : null}

      <ArtifactsSection key={view.run.id} runId={view.run.id} status={view.run.status} />

      {view.approvals.length > 0 || view.run.status === "waiting_approval" ? (
        <Section id="approvals" title="Approvals" count={view.approvals.length}>
          <ApprovalsSection approvals={view.approvals} toolCalls={view.tool_calls} runStatus={view.run.status} onDecided={applyApproval} />
        </Section>
      ) : null}

      <Section title="Timeline">
        <Timeline view={view} active={active} now={now} />
      </Section>

      <Section title="State">
        <StateSection state={view.run.state} />
      </Section>

      {view.agent_messages.length > 0 ? (
        <Section title="Agent messages" count={view.agent_messages.length}>
          <MessagesSection messages={view.agent_messages} />
        </Section>
      ) : null}

      <Section title="Logs" count={view.logs.length}>
        {view.logs.length > 0 ? <LogSection logs={view.logs} /> : <Card><p className="text-sm text-zinc-500">No log lines yet.</p></Card>}
      </Section>
    </div>
  );
}
