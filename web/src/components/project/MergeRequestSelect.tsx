import { useEffect, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { listMergeRequests } from "@/lib/api";
import { requestRef } from "@/lib/mergeRequest";
import { openMergeRequestsKey } from "@/lib/useProjects";
import { Loading, QueryError } from "../QueryState";
import { FieldErrors, SelectField } from "../fields";

/** The most open requests a project's selector lists; the API's own cap. */
const LIMIT = 200;

type Props = {
  /** The project chosen in the form; the selector appears once there is one. */
  projectId: string;
  /** The chosen merge request record's id, or "" for none. */
  value: string;
  onChange: (id: string) => void;
  /** The server's refusals of the choice (the 422 details on `merge_request_id`). */
  errors?: string[];
  /**
   * A record a saved schedule already names. It stays on offer, marked, when it is no longer open, so the form
   * still shows what the schedule updates.
   */
  current?: { id: string; label: string };
  className?: string;
};

/**
 * Chooses the open merge request of the chosen project that a run (or each run of a schedule) works on: the run
 * clones that request's branch and pushes onto it instead of opening a new request (DESIGN 5.22 rule 12). "None" is
 * the default and leaves the request out. A choice that stops being on offer (the request was merged meanwhile) is
 * dropped, so the form never sends an id it does not show.
 */
export default function MergeRequestSelect({ projectId, value, onChange, errors = [], current, className = "" }: Props) {
  const q = useQuery({
    queryKey: openMergeRequestsKey(projectId),
    queryFn: () => listMergeRequests({ project_id: projectId, state: "open", limit: LIMIT }),
    enabled: projectId !== "",
  });
  const open = useMemo(() => (q.data ?? []).filter((mr) => mr.state === "open" && mr.project_id === projectId), [q.data, projectId]);
  const offered = value === "" || open.some((mr) => mr.id === value) || current?.id === value;

  useEffect(() => {
    if (q.data && !offered) onChange("");
  }, [q.data, offered, onChange]);

  if (projectId === "") return null;
  if (q.isLoading) {
    return (
      <div className={className}>
        <Loading label="Loading merge requests…" />
        <FieldErrors errors={errors} />
      </div>
    );
  }
  if (q.error) {
    return (
      <div className={className}>
        <QueryError what="the merge requests" error={q.error} onRetry={() => void q.refetch()} />
        <FieldErrors errors={errors} />
      </div>
    );
  }
  return (
    <SelectField
      className={className}
      label="Merge request"
      value={offered ? value : ""}
      onChange={onChange}
      options={[
        { value: "", label: "None: open a new one" },
        ...open.map((mr) => ({ value: mr.id, label: `${requestRef(mr)} ${mr.title}` })),
        ...(current && !open.some((mr) => mr.id === current.id) ? [{ value: current.id, label: `${current.label} (no longer open)` }] : []),
      ]}
      hint="The run works on the request's branch and pushes onto it."
      errors={errors}
    />
  );
}
