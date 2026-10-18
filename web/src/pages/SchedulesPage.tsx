import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { ApiError, deleteSchedule, listWorkflows, updateSchedule, type Schedule } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { KIND_LABEL, describeWhen, firstDetail, scheduleToInput } from "@/lib/schedule";
import { useProjects } from "@/lib/useProjects";
import { replaceSchedule, schedulesKey, useScheduleEvents, useSchedules } from "@/lib/useSchedules";
import { Badge } from "@/components/Badge";
import Button from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { ConfirmDialog } from "@/components/Modal";
import { EmptyState, InlineError, Loading, QueryError, errorText } from "@/components/QueryState";
import ScheduleDialog from "@/components/schedule/ScheduleDialog";

export default function SchedulesPage() {
  const qc = useQueryClient();
  const q = useSchedules();
  const workflows = useQuery({ queryKey: ["workflows"], queryFn: listWorkflows });
  const projects = useProjects();
  const connection = useScheduleEvents();
  const [editing, setEditing] = useState<Schedule | "new" | null>(null);
  const [deleting, setDeleting] = useState<Schedule | null>(null);

  // The live name wins (a rename shows at once); the stored one covers a workflow that is gone.
  const workflowName = (s: Schedule) => workflows.data?.find((w) => w.id === s.workflow_id)?.name || s.workflow_name || (workflows.data ? "Deleted workflow" : shortId(s.workflow_id));
  const workflowGone = (s: Schedule) => !!workflows.data && !workflows.data.some((w) => w.id === s.workflow_id);
  // The same for the project the runs work on: a schedule without one shows nothing.
  const projectName = (s: Schedule) => (s.project_id ? projects.data?.find((p) => p.id === s.project_id)?.name || s.project_name || shortId(s.project_id) : "");
  const projectGone = (s: Schedule) => !!s.project_id && !!projects.data && !projects.data.some((p) => p.id === s.project_id);

  const toggle = useMutation({
    mutationFn: (s: Schedule) => updateSchedule(s.id, { ...scheduleToInput(s), enabled: !s.enabled }),
    onSuccess: (next) => {
      qc.setQueryData<Schedule[]>(schedulesKey, (old) => replaceSchedule(old, next));
      void qc.invalidateQueries({ queryKey: schedulesKey });
    },
  });

  const remove = useMutation({
    mutationFn: (s: Schedule) => deleteSchedule(s.id),
    onSuccess: () => {
      setDeleting(null);
      void qc.invalidateQueries({ queryKey: schedulesKey });
    },
  });

  return (
    <div>
      <PageHeader title="Schedules">
        <ConnectionIndicator state={connection} />
        <Button variant="primary" onClick={() => setEditing("new")}>
          New schedule
        </Button>
      </PageHeader>

      {toggle.error ? (
        <div className="mb-3">
          <InlineError>Could not change the schedule: {(toggle.error instanceof ApiError && firstDetail(toggle.error.details)) || errorText(toggle.error)}</InlineError>
        </div>
      ) : null}
      {q.isLoading ? <Loading label="Loading schedules…" /> : null}
      {q.error ? <QueryError what="schedules" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No schedules yet. Schedule a workflow to run on a repeating cadence or once at a set time.</EmptyState> : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[66rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Workflow</th>
                <th className="px-4 py-2.5 font-medium">Project</th>
                <th className="px-4 py-2.5 font-medium">Schedule</th>
                <th className="px-4 py-2.5 font-medium">Time zone</th>
                <th className="px-4 py-2.5 font-medium">Next run</th>
                <th className="px-4 py-2.5 font-medium">Last run</th>
                <th className="px-4 py-2.5 font-medium">Enabled</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((s) => {
                const name = workflowName(s);
                const gone = workflowGone(s);
                return (
                  <tr key={s.id} className="align-top hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                    <td className="px-4 py-2.5 font-medium">
                      <div className="flex items-center gap-2">
                        {gone ? (
                          <span>{name}</span>
                        ) : (
                          <Link to={`/workflows/${s.workflow_id}`} className="hover:underline">
                            {name}
                          </Link>
                        )}
                        {gone && s.workflow_name ? <Badge tone="orange">Deleted</Badge> : null}
                      </div>
                    </td>
                    <td className="px-4 py-2.5">
                      {s.project_id ? (
                        <div className="flex items-center gap-2">
                          {projectGone(s) ? (
                            <span>{projectName(s)}</span>
                          ) : (
                            <Link to={`/projects/${encodeURIComponent(s.project_id)}`} className="hover:underline">
                              {projectName(s)}
                            </Link>
                          )}
                          {projectGone(s) ? <Badge tone="orange">Deleted</Badge> : null}
                        </div>
                      ) : (
                        <span className="text-zinc-500">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5">
                      <div className="flex items-center gap-2">
                        <Badge tone="zinc">{KIND_LABEL[s.kind]}</Badge>
                        <span className={s.kind === "cron" ? "font-mono text-xs" : "text-xs"}>{describeWhen(s)}</span>
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{s.tz}</td>
                    <td className="whitespace-nowrap px-4 py-2.5">{s.enabled ? formatDateTime(s.next_run_at) : "—"}</td>
                    <td className="px-4 py-2.5">
                      {s.last_run_at ? (
                        <div className="flex flex-wrap items-center gap-x-2">
                          <span className="whitespace-nowrap">{formatDateTime(s.last_run_at)}</span>
                          {s.last_run_id ? (
                            <Link to={`/runs/${s.last_run_id}`} className="font-mono text-xs underline" title={s.last_run_id}>
                              Run {shortId(s.last_run_id)}
                            </Link>
                          ) : null}
                        </div>
                      ) : (
                        <span className="text-zinc-500">Never</span>
                      )}
                      {s.last_error ? (
                        <p className="mt-1 max-w-xs break-words text-xs text-red-600 dark:text-red-400">{s.last_error}</p>
                      ) : null}
                    </td>
                    <td className="px-4 py-2.5">
                      <input
                        type="checkbox"
                        role="switch"
                        aria-label={`Enable ${name} (${describeWhen(s)})`}
                        checked={s.enabled}
                        disabled={toggle.isPending && toggle.variables?.id === s.id}
                        onChange={() => toggle.mutate(s)}
                        className="size-4 accent-zinc-900 dark:accent-zinc-100"
                      />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-right">
                      <div className="flex justify-end gap-1.5">
                        <Button size="sm" onClick={() => setEditing(s)} aria-label={`Edit ${name} (${describeWhen(s)})`}>
                          Edit
                        </Button>
                        <Button size="sm" variant="ghost" onClick={() => setDeleting(s)} aria-label={`Delete ${name} (${describeWhen(s)})`}>
                          Delete
                        </Button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      {editing ? <ScheduleDialog key={editing === "new" ? "new" : editing.id} schedule={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete the ${workflowName(deleting)} schedule?`}
          message="The schedule stops and is removed. Runs it already started keep their history."
          confirmLabel="Delete schedule"
          busy={remove.isPending}
          error={remove.error ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate(deleting)}
          onCancel={() => {
            remove.reset();
            setDeleting(null);
          }}
        />
      ) : null}
    </div>
  );
}
