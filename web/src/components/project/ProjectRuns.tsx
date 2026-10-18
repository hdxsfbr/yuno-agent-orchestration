import { Link } from "react-router";
import { formatDateTime, shortId } from "@/lib/format";
import { useProjectRuns } from "@/lib/useProjects";
import { RunStatusBadge } from "../Badge";
import { cardClass } from "../Card";
import { Loading, QueryError } from "../QueryState";

/** How many runs the card lists. */
const SHOWN = 10;

/** The project's newest runs, newest first: the top of the list the page reads once for its Merge requests table too. */
export default function ProjectRuns({ projectId }: { projectId: string }) {
  const q = useProjectRuns(projectId);
  const runs = (q.data ?? []).slice(0, SHOWN);
  return (
    <section aria-labelledby="project-runs">
      <h2 id="project-runs" className="mb-2 text-base font-semibold">
        Recent runs
      </h2>
      {q.isLoading ? <Loading label="Loading runs…" /> : null}
      {q.error ? <QueryError what="the runs" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && runs.length === 0 ? <p className={`${cardClass} p-4 text-sm text-zinc-500`}>No runs yet.</p> : null}
      {runs.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Run</th>
                <th className="px-4 py-2.5 font-medium">Status</th>
                <th className="px-4 py-2.5 font-medium">Created</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {runs.map((r) => (
                <tr key={r.id}>
                  <td className="px-4 py-2.5">
                    <Link to={`/runs/${encodeURIComponent(r.id)}`} className="font-mono hover:underline" title={r.id}>
                      {shortId(r.id)}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">
                    <RunStatusBadge status={r.status} />
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(r.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}
