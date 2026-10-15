import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { createRun, type Agent } from "@/lib/api";
import Button from "./Button";
import { Modal } from "./Modal";
import { InlineError, errorText } from "./QueryState";
import { control } from "./fields";

/** Starts a run of a single agent with a task and opens the run page. */
export default function QuickRunDialog({ agent, onClose }: { agent: Pick<Agent, "id" | "name">; onClose: () => void }) {
  const [task, setTask] = useState("");
  const navigate = useNavigate();
  const qc = useQueryClient();
  const start = useMutation({
    mutationFn: () => createRun({ agent_id: agent.id, task: task.trim() }),
    onSuccess: (run) => {
      void qc.invalidateQueries({ queryKey: ["runs"] });
      navigate(`/runs/${run.id}`);
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (task.trim() !== "" && !start.isPending) start.mutate();
  };

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
        {start.error ? (
          <div className="mt-3">
            <InlineError>{errorText(start.error)}</InlineError>
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
