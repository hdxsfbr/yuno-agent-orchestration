import { Link } from "react-router";
import { shortId } from "@/lib/format";
import type { Run } from "@/lib/api";

/** Whether the run works on a project. */
export const hasProject = (run: Pick<Run, "project_id" | "project">): boolean => !!run.project_id || !!run.project;

/**
 * The project a run works on: the name it had when the run was queued, linking to the
 * project's page. Renders nothing for a run without a project.
 */
export default function ProjectLink({ run, className = "hover:underline" }: { run: Pick<Run, "project_id" | "project">; className?: string }) {
  const name = run.project?.name ?? shortId(run.project_id);
  if (!run.project_id) return run.project ? <>{name}</> : null;
  return (
    <Link to={`/projects/${encodeURIComponent(run.project_id)}`} className={className}>
      {name}
    </Link>
  );
}
