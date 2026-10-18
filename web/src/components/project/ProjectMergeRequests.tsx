import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { listMergeRequests, type MergeRequest } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { requestNoun, requestRef } from "@/lib/mergeRequest";
import { projectMergeRequestsKey, useProjectRuns } from "@/lib/useProjects";
import { cardClass } from "../Card";
import { CIStatusBadge, MergeRequestStateBadge } from "../MergeRequestBadges";
import { Loading, QueryError } from "../QueryState";

/** "+2 updates" linking to the runs that updated the request; nothing when none did. */
function updatedBy(mr: MergeRequest, count: number) {
  if (count === 0) return null;
  return (
    <Link to={`/runs?merge_request_id=${encodeURIComponent(mr.id)}`} className="text-xs underline" title="Runs that updated this request">
      +{count} {count === 1 ? "update" : "updates"}
    </Link>
  );
}

/** The merge and pull requests the platform opened for the project's runs, newest first. */
export default function ProjectMergeRequests({ projectId }: { projectId: string }) {
  const q = useQuery({ queryKey: projectMergeRequestsKey(projectId), queryFn: () => listMergeRequests({ project_id: projectId }) });
  const requests = q.data ?? [];
  // How many runs updated each request, counted from the project's newest runs, which the page reads once for its Recent runs card too.
  const runs = useProjectRuns(projectId);
  const openedBy = new Map(requests.map((mr) => [mr.id, mr.run_id]));
  const updates = new Map<string, number>();
  for (const r of runs.data ?? []) {
    if (r.merge_request_id && r.id !== openedBy.get(r.merge_request_id)) updates.set(r.merge_request_id, (updates.get(r.merge_request_id) ?? 0) + 1);
  }
  return (
    <section aria-labelledby="project-merge-requests">
      <h2 id="project-merge-requests" className="mb-2 text-base font-semibold">
        Merge requests
        {q.data ? <span className="ml-2 text-sm font-normal text-zinc-500">{requests.length}</span> : null}
      </h2>
      {q.isLoading ? <Loading label="Loading merge requests…" /> : null}
      {q.error ? <QueryError what="the merge requests" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && requests.length === 0 ? <p className={`${cardClass} p-4 text-sm text-zinc-500`}>None yet.</p> : null}
      {requests.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[44rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Request</th>
                <th className="px-4 py-2.5 font-medium">Title</th>
                <th className="px-4 py-2.5 font-medium">Runs</th>
                <th className="px-4 py-2.5 font-medium">State</th>
                <th className="px-4 py-2.5 font-medium">CI</th>
                <th className="px-4 py-2.5 font-medium">Unresolved threads</th>
                <th className="px-4 py-2.5 font-medium">Updated</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {requests.map((mr) => (
                <tr key={mr.id}>
                  <td className="whitespace-nowrap px-4 py-2.5">
                    <a href={mr.url} target="_blank" rel="noreferrer" className="font-mono underline" title={`Open the ${requestNoun(mr.provider).toLowerCase()} on the provider`}>
                      {requestRef(mr)}
                    </a>
                  </td>
                  <td className="max-w-xs px-4 py-2.5">
                    <span className="block truncate" title={mr.title}>
                      {mr.title}
                    </span>
                  </td>
                  <td className="px-4 py-2.5">
                    <div className="flex flex-wrap items-center gap-x-2">
                      <Link to={`/runs/${encodeURIComponent(mr.run_id)}`} className="font-mono hover:underline" title={mr.run_id}>
                        {shortId(mr.run_id)}
                      </Link>
                      {updatedBy(mr, updates.get(mr.id) ?? 0)}
                    </div>
                  </td>
                  <td className="px-4 py-2.5">
                    <MergeRequestStateBadge state={mr.state} />
                  </td>
                  <td className="px-4 py-2.5">
                    <CIStatusBadge status={mr.ci_status} />
                  </td>
                  <td className="px-4 py-2.5 tabular-nums">{mr.unresolved_threads == null ? <span title="The provider could not tell">Unknown</span> : mr.unresolved_threads}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(mr.updated_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}
