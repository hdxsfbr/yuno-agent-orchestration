import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, pollIntake, type Project } from "@/lib/api";
import { formatAgo } from "@/lib/format";
import { projectsKey } from "@/lib/useProjects";
import { useNow } from "@/lib/useNow";
import Button from "../Button";
import { Card } from "../Card";
import { InlineError, errorText } from "../QueryState";

/**
 * The project's issue intake: whether it is on, with which label and workflow, when it last polled, what the last poll
 * could not do, and a way to poll now. A poll shows how many runs it started and how many finished issue runs it
 * reported; the project and its runs are read again afterwards.
 */
export default function IntakeCard({ project }: { project: Project }) {
  const qc = useQueryClient();
  const now = useNow(true, 10_000);
  const intake = project.intake;
  const enabled = !!intake?.enabled;

  const poll = useMutation({
    mutationFn: () => pollIntake(project.id),
    // A refusal can mean the settings changed meanwhile (the intake was switched off), so the project is read again then too.
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: projectsKey });
      void qc.invalidateQueries({ queryKey: ["runs"] });
    },
  });
  // The server's message for the intake being off is the whole story; any other failure says what was being done.
  const refused = poll.error instanceof ApiError && poll.error.status === 409;

  return (
    <section aria-labelledby="project-intake">
      <h2 id="project-intake" className="mb-2 text-base font-semibold">
        Issue intake
      </h2>
      <Card>
        <p className="text-lg">{enabled ? `On: label ${intake?.label}, ${intake?.workflow_name ? `workflow ${intake.workflow_name}` : "no workflow"}` : "Off"}</p>
        <p className="mt-1 text-sm text-zinc-500">{intake?.polled_at ? `Last poll ${formatAgo(intake.polled_at, now)}` : "Not polled yet"}</p>
        {intake?.last_error ? <p className="mt-1 break-words text-xs text-zinc-500">{intake.last_error}</p> : null}
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <Button size="sm" onClick={() => poll.mutate()} disabled={!enabled || poll.isPending}>
            {poll.isPending ? "Polling…" : "Poll now"}
          </Button>
          {poll.data ? (
            <span role="status" className="text-sm">
              Took {poll.data.taken}, closed {poll.data.closed}
            </span>
          ) : null}
        </div>
        {poll.error ? (
          <div className="mt-3">
            <InlineError>{refused ? errorText(poll.error) : `Could not poll the project: ${errorText(poll.error)}`}</InlineError>
          </div>
        ) : null}
      </Card>
    </section>
  );
}
