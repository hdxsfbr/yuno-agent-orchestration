import { Fragment, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Navigate, useNavigate, useParams } from "react-router";
import { deleteProject, type Project, type ProjectLastRun } from "@/lib/api";
import { providerLabel, repoDisplay, toolchainLabel } from "@/lib/project";
import { projectsKey, useProjects } from "@/lib/useProjects";
import { Badge, RunStatusBadge } from "@/components/Badge";
import Button, { buttonClass } from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import { CheckNote, useProjectChecks } from "@/components/project/CheckResult";
import ProjectDialog from "@/components/project/ProjectDialog";
import SpendCell from "@/components/project/SpendCell";
import { EmptyState, Loading, QueryError, errorText } from "@/components/QueryState";

const COLUMNS = 8;

/** The toolchain the last run's checkout used, and how that run is doing. */
function LastRunCell({ run }: { run?: ProjectLastRun }) {
  if (!run) return <span className="text-zinc-500">—</span>;
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      {run.toolchain ? <Badge title="Toolchain of the last run's checkout">{toolchainLabel(run.toolchain)}</Badge> : null}
      <Link to={`/runs/${encodeURIComponent(run.id)}`} title="Open the last run" className="rounded-full">
        <RunStatusBadge status={run.status} />
      </Link>
    </span>
  );
}

export default function ProjectsPage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const q = useProjects();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Project | null>(null);
  const checks = useProjectChecks();

  const remove = useMutation({
    mutationFn: (p: Project) => deleteProject(p.id),
    onSuccess: (_, p) => {
      setDeleting(null);
      checks.forget(p.id);
      void qc.invalidateQueries({ queryKey: projectsKey });
    },
  });

  // /projects/<id>/edit opens that project's form over the list; an id that names no project leads back to the list.
  const editing = id ? q.data?.find((p) => p.id === id) : undefined;
  if (id && q.data && !editing) return <Navigate to="/projects" replace />;

  return (
    <div>
      <PageHeader title="Projects">
        <Button variant="primary" onClick={() => setCreating(true)}>
          New project
        </Button>
      </PageHeader>

      {q.isLoading ? <Loading label="Loading projects…" /> : null}
      {q.error ? <QueryError what="projects" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No projects yet. Add a git repository for agents to work on.</EmptyState> : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[64rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Repository</th>
                <th className="px-4 py-2.5 font-medium">Provider</th>
                <th className="px-4 py-2.5 font-medium">Default branch</th>
                <th className="px-4 py-2.5 font-medium">Last run</th>
                <th className="px-4 py-2.5 font-medium">Spend</th>
                <th className="px-4 py-2.5 font-medium">Sandbox network</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((p) => {
                const running = checks.running(p.id);
                const shown = checks.shownFor(p);
                return (
                  <Fragment key={p.id}>
                    <tr className="border-t border-zinc-200 first:border-t-0 hover:bg-zinc-50 dark:border-zinc-800 dark:hover:bg-zinc-800/40">
                      <td className="px-4 py-2.5">
                        <Link to={`/projects/${encodeURIComponent(p.id)}`} className="font-medium hover:underline">
                          {p.name}
                        </Link>
                        {p.description ? (
                          <p className="mt-0.5 max-w-xs truncate text-xs text-zinc-500" title={p.description}>
                            {p.description}
                          </p>
                        ) : null}
                      </td>
                      <td className="px-4 py-2.5 font-mono text-xs break-all" title={p.repo_url}>
                        {repoDisplay(p.repo_url)}
                      </td>
                      <td className="px-4 py-2.5">{providerLabel(p.provider)}</td>
                      <td className="px-4 py-2.5 font-mono text-xs">{p.default_branch}</td>
                      <td className="px-4 py-2.5">
                        <LastRunCell run={p.last_run} />
                      </td>
                      <td className="px-4 py-2.5">
                        <SpendCell project={p} />
                      </td>
                      <td className="px-4 py-2.5">
                        <Badge tone={p.sandbox_network ? "amber" : "zinc"} title={p.sandbox_network ? "Agents can install dependencies in the sandbox" : "The sandbox has no network access"}>
                          {p.sandbox_network ? "Network on" : "Network off"}
                        </Badge>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-right">
                        <div className="flex justify-end gap-1.5">
                          <Button size="sm" onClick={() => void checks.check(p)} disabled={running} aria-label={`Check ${p.name}`}>
                            {running ? "Checking…" : "Check"}
                          </Button>
                          <Link to={`/projects/${encodeURIComponent(p.id)}/edit`} className={buttonClass("secondary", "sm")} aria-label={`Edit ${p.name}`}>
                            Edit
                          </Link>
                          <Button size="sm" variant="ghost" onClick={() => setDeleting(p)} aria-label={`Delete ${p.name}`}>
                            Delete
                          </Button>
                        </div>
                      </td>
                    </tr>
                    {shown ? (
                      <tr>
                        <td colSpan={COLUMNS} className="px-4 pb-3">
                          <CheckNote result={shown} />
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      {creating ? <ProjectDialog onClose={() => setCreating(false)} /> : null}
      {editing ? <ProjectDialog key={editing.id} project={editing} onClose={() => void navigate("/projects")} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          message="Runs that used this project keep their history."
          confirmLabel="Delete project"
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
