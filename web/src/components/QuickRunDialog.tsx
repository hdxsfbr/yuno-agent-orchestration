import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { ApiError, createRun, type Agent } from "@/lib/api";
import { errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { projectsKey, useProjects } from "@/lib/useProjects";
import Button from "./Button";
import { Modal } from "./Modal";
import { InlineError, errorText } from "./QueryState";
import { SelectField, control } from "./fields";

/** Starts a run of a single agent with a task and opens the run page. */
export default function QuickRunDialog({ agent, onClose }: { agent: Pick<Agent, "id" | "name">; onClose: () => void }) {
  const [task, setTask] = useState("");
  const [projectChoice, setProjectChoice] = useState("");
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);
  const navigate = useNavigate();
  const qc = useQueryClient();
  // The project choice exists only once there is a project to choose.
  const projects = useProjects();
  const list = projects.data ?? [];
  // A project deleted since it was chosen no longer counts as chosen.
  const projectId = list.some((p) => p.id === projectChoice) ? projectChoice : "";
  const start = useMutation({
    mutationFn: () => createRun({ agent_id: agent.id, task: task.trim(), ...(projectId ? { project_id: projectId } : {}) }),
    onSuccess: (run) => {
      void qc.invalidateQueries({ queryKey: ["runs"] });
      navigate(`/runs/${run.id}`);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
        // A refused project may be one that was just deleted: show the current list.
        void qc.invalidateQueries({ queryKey: projectsKey });
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (task.trim() === "" || start.isPending) return;
    setServerErrors({});
    setFormError(null);
    start.mutate();
  };

  // The select shows its own errors; when it is gone (the last project was deleted) they are listed below like the rest.
  const selectShown = list.length > 0;
  const projectErrors = selectShown ? errorsFor(serverErrors, "project_id") : [];
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !(selectShown && k.split(".")[0] === "project_id"))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  const messages = formError && Object.keys(serverErrors).length === 0 ? [formError] : unplaced;

  return (
    <Modal title={`Run ${agent.name}`} onClose={start.isPending ? () => undefined : onClose}>
      <form onSubmit={submit}>
        <label htmlFor="quick-run-task" className="mb-1 block text-sm font-medium">
          Task
        </label>
        <textarea
          id="quick-run-task"
          autoFocus
          rows={6}
          value={task}
          onChange={(e) => setTask(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit(e);
          }}
          placeholder="What should the agent do?"
          className={control(false)}
        />
        {selectShown ? (
          <SelectField
            className="mt-3"
            label="Project"
            value={projectId}
            onChange={setProjectChoice}
            options={[{ value: "", label: "None" }, ...list.map((p) => ({ value: p.id, label: p.name }))]}
            hint="Choose one to have the agent work on a checkout of its repository."
            errors={projectErrors}
          />
        ) : null}
        {messages.length > 0 ? (
          <div className="mt-3 space-y-1">
            {messages.map((m, i) => (
              <InlineError key={i}>{m}</InlineError>
            ))}
          </div>
        ) : null}
        <div className="mt-4 flex justify-end gap-2">
          <Button onClick={onClose} disabled={start.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={task.trim() === "" || start.isPending}>
            {start.isPending ? "Starting…" : "Start run"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
