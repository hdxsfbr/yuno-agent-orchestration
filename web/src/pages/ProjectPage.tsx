import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Navigate, useNavigate, useParams } from "react-router";
import { deleteProject } from "@/lib/api";
import { providerLabel, repoDisplay } from "@/lib/project";
import { projectsKey, useProjects } from "@/lib/useProjects";
import { Badge } from "@/components/Badge";
import Button from "@/components/Button";
import { Card } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import BudgetCard from "@/components/project/BudgetCard";
import { CheckNote, useProjectChecks } from "@/components/project/CheckResult";
import IntakeCard from "@/components/project/IntakeCard";
import ProjectDialog from "@/components/project/ProjectDialog";
import ProjectMergeRequests from "@/components/project/ProjectMergeRequests";
import ProjectNotes from "@/components/project/ProjectNotes";
import ProjectRuns from "@/components/project/ProjectRuns";
import { Loading, QueryError, errorText } from "@/components/QueryState";

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-zinc-500">{label}</dt>
      <dd className="mt-0.5 text-sm">{children}</dd>
    </div>
  );
}

/** One project: its settings, budget, instructions, notes, merge requests and newest runs. */
export default function ProjectPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const q = useProjects();
  const checks = useProjectChecks();
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const remove = useMutation({
    mutationFn: () => deleteProject(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: projectsKey });
      void navigate("/projects");
    },
  });

  const project = q.data?.find((p) => p.id === id);
  // An id that names no project leads back to the list, once the list is known.
  if (q.data && !project) return <Navigate to="/projects" replace />;

  const back = (
    <Link to="/projects" className="text-sm text-zinc-500 hover:underline">
      ← Projects
    </Link>
  );

  if (!project) {
    return (
      <div className="space-y-3">
        {back}
        {q.error ? <QueryError what="the project" error={q.error} onRetry={() => void q.refetch()} /> : <Loading label="Loading project…" />}
      </div>
    );
  }

  const running = checks.running(project.id);
  const shown = checks.shownFor(project);

  return (
    <div className="max-w-5xl space-y-5">
      {back}

      <header>
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="text-xl font-semibold">{project.name}</h1>
          <Badge>{providerLabel(project.provider)}</Badge>
        </div>
        <p className="mt-1 break-all font-mono text-xs text-zinc-500">
          <a href={project.repo_url} target="_blank" rel="noreferrer" className="hover:underline" title={project.repo_url}>
            {repoDisplay(project.repo_url)}
          </a>
        </p>
      </header>

      <section aria-labelledby="project-settings">
        <h2 id="project-settings" className="mb-2 text-base font-semibold">
          Settings
        </h2>
        <Card>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <dl className="grid min-w-0 flex-1 gap-4 sm:grid-cols-3">
              <Item label="Default branch">
                <span className="font-mono text-xs">{project.default_branch}</span>
              </Item>
              <Item label="Sandbox network">
                <Badge tone={project.sandbox_network ? "amber" : "zinc"} title={project.sandbox_network ? "Agents can install dependencies in the sandbox" : "The sandbox has no network access"}>
                  {project.sandbox_network ? "Network on" : "Network off"}
                </Badge>
              </Item>
              <Item label="Description">
                {project.description ? <span className="whitespace-pre-wrap break-words">{project.description}</span> : <span className="text-zinc-500">No description.</span>}
              </Item>
            </dl>
            <div className="flex gap-1.5">
              <Button size="sm" onClick={() => setEditing(true)}>
                Edit
              </Button>
              <Button size="sm" onClick={() => void checks.check(project)} disabled={running}>
                {running ? "Checking…" : "Check"}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setDeleting(true)}>
                Delete
              </Button>
            </div>
          </div>
          {shown ? (
            <div className="mt-3">
              <CheckNote result={shown} />
            </div>
          ) : null}
        </Card>
      </section>

      <div className="grid items-start gap-5 md:grid-cols-2">
        <BudgetCard project={project} />
        <IntakeCard project={project} />
      </div>

      <section aria-labelledby="project-instructions">
        <h2 id="project-instructions" className="mb-2 text-base font-semibold">
          Instructions
        </h2>
        <Card>
          {project.instructions ? (
            <p className="whitespace-pre-wrap break-words text-sm">{project.instructions}</p>
          ) : (
            <p className="text-sm text-zinc-500">No instructions yet.</p>
          )}
        </Card>
      </section>

      <ProjectNotes projectId={project.id} />
      <ProjectMergeRequests projectId={project.id} />
      <ProjectRuns projectId={project.id} />

      {editing ? <ProjectDialog key={project.id} project={project} onClose={() => setEditing(false)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${project.name}?`}
          message="Runs that used this project keep their history."
          confirmLabel="Delete project"
          busy={remove.isPending}
          error={remove.error ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate()}
          onCancel={() => {
            remove.reset();
            setDeleting(false);
          }}
        />
      ) : null}
    </div>
  );
}
