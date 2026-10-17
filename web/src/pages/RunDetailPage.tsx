import type { ReactNode } from "react";
import { Link, useParams } from "react-router";
import { isTerminalStatus } from "@/lib/api";
import { useNow } from "@/lib/useNow";
import { useRunTrace } from "@/lib/useRunTrace";
import { Card } from "@/components/Card";
import { Loading, QueryError } from "@/components/QueryState";
import ApprovalsSection from "@/components/run/ApprovalsSection";
import LogSection from "@/components/run/LogSection";
import MessagesSection from "@/components/run/MessagesSection";
import RunHeader from "@/components/run/RunHeader";
import StateSection from "@/components/run/StateSection";
import Timeline from "@/components/run/Timeline";

function Section({ title, count, children }: { title: string; count?: number; children: ReactNode }) {
  return (
    <section>
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
  const { view, error, connection, retry, applyRun, applyApproval } = useRunTrace(id);
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
      <RunHeader run={view.run} connection={connection} now={now} toolCalls={view.tool_calls} onRunChange={applyRun} />

      {view.approvals.length > 0 || view.run.status === "waiting_approval" ? (
        <Section title="Approvals" count={view.approvals.length}>
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
