import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { cancelPlan, getPlan, listApprovals, listMergeRequests, listProjectPlans, pauseWaves, resumeWaves, type PlanEvent, type PlanMain, type PlanTicket, type Project } from "@/lib/api";
import { formatAgo, formatDateTime, shortId } from "@/lib/format";
import { eventText, isOpenPlan, MAIN_STATE_LABEL, MAIN_STATE_TONE, PLAN_STATUS_LABEL, PLAN_STATUS_TONE, planWaves, TICKET_STATUS_LABEL, TICKET_STATUS_TONE, waitingLabel, WAVE_STATE_LABEL, WAVE_STATE_TONE, type WaveView } from "@/lib/plan";
import { requestRef } from "@/lib/mergeRequest";
import { REFRESH_DELAY_MS } from "@/lib/useRefreshOnEvents";
import { useEventStream } from "@/lib/useEventStream";
import { projectMergeRequestsKey, projectsKey } from "@/lib/useProjects";
import { Badge, RunStatusBadge } from "../Badge";
import Button from "../Button";
import { Card, cardClass } from "../Card";
import ConnectionIndicator from "../ConnectionIndicator";
import { TextAreaField } from "../fields";
import { Modal } from "../Modal";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";

const plansKey = (projectId: string) => ["plans", "project", projectId] as const;
const planKey = (planId: string) => ["plans", "detail", planId] as const;

/** The events that change what the section shows. */
const PLAN_EVENTS = ["plan.updated", "run.status"];

function PauseDialog({ projectId, onClose }: { projectId: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [reason, setReason] = useState("");
  const pause = useMutation({
    mutationFn: () => pauseWaves(projectId, reason.trim()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: projectsKey });
      void qc.invalidateQueries({ queryKey: ["plans"] });
      onClose();
    },
  });
  return (
    <Modal title="Pause waves" onClose={pause.isPending ? () => undefined : onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (reason.trim()) pause.mutate();
        }}
      >
        <p className="mb-3 text-sm text-zinc-600 dark:text-zinc-400">Nothing is released and no ship run starts until you resume. Runs already working finish.</p>
        <TextAreaField label="Reason" value={reason} onChange={setReason} rows={3} required />
        {pause.error ? (
          <div className="mt-3">
            <InlineError>{errorText(pause.error)}</InlineError>
          </div>
        ) : null}
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={onClose} disabled={pause.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={pause.isPending || !reason.trim()}>
            {pause.isPending ? "Pausing…" : "Pause waves"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function CancelPlanDialog({ planId, feature, onClose }: { planId: string; feature: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [reason, setReason] = useState("");
  const cancel = useMutation({
    mutationFn: () => cancelPlan(planId, reason.trim()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["plans"] });
      onClose();
    },
  });
  return (
    <Modal title="Cancel plan" onClose={cancel.isPending ? () => undefined : onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (reason.trim()) cancel.mutate();
        }}
      >
        <p className="mb-3 text-sm text-zinc-600 dark:text-zinc-400">{`Cancel the plan for ${feature}? No more tickets are released, and the feature name can be used for a new plan.`}</p>
        <TextAreaField label="Reason" value={reason} onChange={setReason} rows={3} required />
        {cancel.error ? (
          <div className="mt-3">
            <InlineError>{errorText(cancel.error)}</InlineError>
          </div>
        ) : null}
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={onClose} disabled={cancel.isPending}>
            Keep plan
          </Button>
          <Button type="submit" variant="primary" disabled={cancel.isPending || !reason.trim()}>
            {cancel.isPending ? "Cancelling…" : "Cancel plan"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function TicketRow({ t, mr }: { t: PlanTicket; mr?: { url: string; ref: string; draft: boolean } }) {
  return (
    <tr>
      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-xs">{t.ticket_id}</td>
      <td className="max-w-xs px-4 py-2.5">
        <span className="block truncate" title={t.title}>
          {t.title}
        </span>
      </td>
      <td className="whitespace-nowrap px-4 py-2.5">
        {t.issue_url ? (
          <a href={t.issue_url} target="_blank" rel="noreferrer" className="font-mono underline" title="Open the issue on the provider">
            {t.issue_number != null ? `#${t.issue_number}` : "Issue"}
          </a>
        ) : (
          <span className="text-zinc-500">None</span>
        )}
      </td>
      <td className="whitespace-nowrap px-4 py-2.5">
        {t.run_id ? (
          <Link to={`/runs/${encodeURIComponent(t.run_id)}`} className="font-mono hover:underline" title={t.run_id}>
            {shortId(t.run_id)}
          </Link>
        ) : (
          <span className="text-zinc-500">None</span>
        )}
      </td>
      <td className="whitespace-nowrap px-4 py-2.5">
        {mr ? (
          <span className="flex items-center gap-2">
            <a href={mr.url} target="_blank" rel="noreferrer" className="font-mono underline" title="Open the request on the provider">
              {mr.ref}
            </a>
            {mr.draft ? <Badge tone="amber">Draft</Badge> : null}
          </span>
        ) : (
          <span className="text-zinc-500">None</span>
        )}
      </td>
      <td className="px-4 py-2.5">
        <Badge tone={TICKET_STATUS_TONE[t.status] ?? "zinc"}>{TICKET_STATUS_LABEL[t.status] ?? t.status}</Badge>
      </td>
    </tr>
  );
}

/** How main's pipeline stands: its state, the commit it was read at and when, linked to the pipeline when the provider gave an address. */
function MainStatus({ main }: { main: PlanMain | null | undefined }) {
  if (!main) return <p className="text-sm text-zinc-500">Main has not been checked yet.</p>;
  const label = (
    <>
      {main.sha ? <span className="font-mono text-xs">{main.sha.slice(0, 8)}</span> : null}
      {main.at ? <span className="text-xs text-zinc-500">{formatAgo(main.at)}</span> : null}
    </>
  );
  return (
    <p className="flex flex-wrap items-center gap-2 text-sm">
      <span className="font-medium">Main</span>
      <Badge tone={MAIN_STATE_TONE[main.state] ?? "zinc"} pulse={main.state === "running"}>
        {MAIN_STATE_LABEL[main.state] ?? main.state}
      </Badge>
      {main.url ? (
        <a href={main.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-2 underline" title="Open the pipeline on the provider">
          {label}
        </a>
      ) : (
        label
      )}
    </p>
  );
}

/** A wave's ship run, linked to the run where the operator answers; when the run waits, the approval it waits on; when it ended without shipping, why the wave is held. */
function ShipRun({ w, pausedReason }: { w: WaveView; pausedReason?: string }) {
  const runId = w.shipRun?.run_id ?? "";
  // Which approval the run waits on: its pending one, read only while the run waits and cached.
  const pending = useQuery({ queryKey: ["approvals", "pending", "run", runId], queryFn: () => listApprovals({ status: "pending", runId }), enabled: !!w.waiting, staleTime: 15_000 });
  if (!w.shipRun) return null;
  const href = `/runs/${encodeURIComponent(w.shipRun.run_id)}`;
  return (
    <p className="mb-1.5 flex flex-wrap items-center gap-2 text-sm">
      <span className="text-zinc-500">Ship run</span>
      <Link to={href} className="font-mono hover:underline" title={w.shipRun.run_id}>
        {shortId(w.shipRun.run_id)}
      </Link>
      <RunStatusBadge status={w.shipRun.status} />
      {w.waiting ? (
        <Link to={href} className="font-medium text-amber-800 underline dark:text-amber-300">
          {waitingLabel(pending.data?.[0]?.node_id)}
        </Link>
      ) : null}
      {w.state === "held" && pausedReason ? <span className="text-amber-800 dark:text-amber-300">{pausedReason}</span> : null}
    </p>
  );
}

function EventList({ events }: { events: PlanEvent[] }) {
  if (events.length === 0) return <p className="text-sm text-zinc-500">Nothing has happened yet.</p>;
  return (
    <ul className="divide-y divide-zinc-200 dark:divide-zinc-800">
      {events.map((e) => {
        const { label, detail } = eventText(e);
        return (
          <li key={e.id} className="py-2 first:pt-0 last:pb-0 text-sm">
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <span className={e.kind === "escalated" ? "font-medium text-red-700 dark:text-red-400" : "font-medium"}>{label}</span>
              <span className="text-xs text-zinc-500">{formatDateTime(e.at)}</span>
            </div>
            {detail ? <p className="mt-0.5 whitespace-pre-wrap break-words text-zinc-600 dark:text-zinc-400">{detail}</p> : null}
          </li>
        );
      })}
    </ul>
  );
}

/** The project's plans: the open one with its tickets by wave and its events, the older ones below, with pause and resume for the waves. */
export default function ProjectPlans({ project }: { project: Project }) {
  const qc = useQueryClient();
  const [pausing, setPausing] = useState(false);
  const [cancelling, setCancelling] = useState(false);

  const plans = useQuery({ queryKey: plansKey(project.id), queryFn: () => listProjectPlans(project.id) });
  const open = plans.data?.find(isOpenPlan);
  const older = (plans.data ?? []).filter((p) => p !== open);
  const detail = useQuery({ queryKey: planKey(open?.id ?? ""), queryFn: () => getPlan(open!.id), enabled: !!open });
  // The same list the Merge requests table reads, for each ticket's request link and its Draft badge.
  const requests = useQuery({ queryKey: projectMergeRequestsKey(project.id), queryFn: () => listMergeRequests({ project_id: project.id }) });
  const mrById = new Map((requests.data ?? []).map((mr) => [mr.id, { url: mr.url, ref: requestRef(mr), draft: !!mr.draft }]));

  // Live: a plan or run change refreshes the section after a short gather delay.
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const connection = useEventStream("/api/events", (e) => {
    if (!PLAN_EVENTS.includes(e.event) || timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = undefined;
      void qc.invalidateQueries({ queryKey: ["plans"] });
      void qc.invalidateQueries({ queryKey: projectsKey });
      void qc.invalidateQueries({ queryKey: projectMergeRequestsKey(project.id) });
    }, REFRESH_DELAY_MS);
  });

  const resume = useMutation({
    mutationFn: () => resumeWaves(project.id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: projectsKey });
      void qc.invalidateQueries({ queryKey: ["plans"] });
    },
  });

  const paused = !!project.waves_paused;
  const reason = project.waves_paused_reason;
  const pausedBadge = paused ? (
    <Badge tone="amber" title={reason}>
      Paused
    </Badge>
  ) : null;

  return (
    <section aria-labelledby="project-plans">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 id="project-plans" className="text-base font-semibold">
          Plans
        </h2>
        <div className="flex items-center gap-3">
          <ConnectionIndicator state={connection} />
          {paused ? (
            <Button size="sm" onClick={() => resume.mutate()} disabled={resume.isPending}>
              {resume.isPending ? "Resuming…" : "Resume waves"}
            </Button>
          ) : (
            <Button size="sm" onClick={() => setPausing(true)}>
              Pause waves
            </Button>
          )}
        </div>
      </div>
      {resume.error ? (
        <div className="mb-2">
          <InlineError>{`Could not resume the waves: ${errorText(resume.error)}`}</InlineError>
        </div>
      ) : null}

      {plans.isLoading ? <Loading label="Loading plans…" /> : null}
      {plans.error ? <QueryError what="the plans" error={plans.error} onRetry={() => void plans.refetch()} /> : null}

      {paused && !open ? (
        <Card className="mb-3 p-4">
          <p className="flex flex-wrap items-center gap-2 text-sm">
            {pausedBadge}
            {reason ? <span className="break-words">{reason}</span> : null}
          </p>
        </Card>
      ) : null}

      {plans.data && !open ? <p className={`${cardClass} p-4 text-sm text-zinc-500`}>No plan is open. A plan appears here once its tickets are published.</p> : null}

      {open ? (
        <Card className="space-y-4">
          <div>
            <div className="flex flex-wrap items-center gap-2">
              <h3 className="min-w-0 break-words text-sm font-semibold">{open.feature}</h3>
              <Badge tone={PLAN_STATUS_TONE[open.status] ?? "zinc"}>{PLAN_STATUS_LABEL[open.status] ?? open.status}</Badge>
              {pausedBadge}
              <span className="ml-auto">
                <Button size="sm" onClick={() => setCancelling(true)}>
                  Cancel plan
                </Button>
              </span>
            </div>
            <p className="mt-1 text-xs text-zinc-500">
              {`Current wave ${open.current_wave} · Updated ${formatDateTime(open.updated_at)}`}
            </p>
            {paused && reason ? <p className="mt-1 whitespace-pre-wrap break-words text-sm text-amber-800 dark:text-amber-300">{reason}</p> : null}
          </div>

          {detail.isLoading ? <Loading label="Loading tickets…" /> : null}
          {detail.error ? <QueryError what="the plan" error={detail.error} onRetry={() => void detail.refetch()} /> : null}
          {detail.data ? (
            <>
              <MainStatus main={detail.data.main} />
              {planWaves(detail.data).map((g) => (
                <div key={g.wave}>
                  <h4 className="mb-1 flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
                    <span>
                      {`Wave ${g.wave}`}
                      {g.wave === open.current_wave ? " · current" : ""}
                    </span>
                    <Badge tone={WAVE_STATE_TONE[g.state]}>{WAVE_STATE_LABEL[g.state]}</Badge>
                  </h4>
                  <ShipRun w={g} pausedReason={paused ? reason : undefined} />
                  <div className="overflow-x-auto rounded-md border border-zinc-200 dark:border-zinc-800">
                    <table className="w-full min-w-[40rem] text-left text-sm">
                      <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
                        <tr>
                          <th className="px-4 py-2 font-medium">Ticket</th>
                          <th className="px-4 py-2 font-medium">Title</th>
                          <th className="px-4 py-2 font-medium">Issue</th>
                          <th className="px-4 py-2 font-medium">Run</th>
                          <th className="px-4 py-2 font-medium">Request</th>
                          <th className="px-4 py-2 font-medium">Status</th>
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
                        {g.tickets.map((t) => (
                          <TicketRow key={t.ticket_id} t={t} mr={t.merge_request_id ? mrById.get(t.merge_request_id) : undefined} />
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              ))}
              <div>
                <h4 className="mb-1 text-xs font-medium uppercase tracking-wide text-zinc-500">Events</h4>
                <EventList events={detail.data.events} />
              </div>
            </>
          ) : null}
        </Card>
      ) : null}

      {older.length > 0 ? (
        <div className="mt-3">
          <h3 className="mb-1 text-sm font-medium text-zinc-500">Older plans</h3>
          <ul className={`${cardClass} divide-y divide-zinc-200 dark:divide-zinc-800`}>
            {older.map((p) => (
              <li key={p.id} className="flex flex-wrap items-center justify-between gap-2 px-4 py-2.5 text-sm">
                <span className="min-w-0 break-words">{p.feature}</span>
                <span className="flex items-center gap-3">
                  <span className="text-xs text-zinc-500">{formatDateTime(p.updated_at)}</span>
                  <Badge tone={PLAN_STATUS_TONE[p.status] ?? "zinc"}>{PLAN_STATUS_LABEL[p.status] ?? p.status}</Badge>
                </span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {cancelling && open ? <CancelPlanDialog planId={open.id} feature={open.feature} onClose={() => setCancelling(false)} /> : null}
      {pausing ? <PauseDialog projectId={project.id} onClose={() => setPausing(false)} /> : null}
    </section>
  );
}
