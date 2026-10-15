import { useState } from "react";
import type { Agent, WorkflowDefinition, WorkflowNode } from "@/lib/api";
import { describeCondition } from "@/lib/condition";
import { errorsFor, type FieldErrors } from "@/lib/agentForm";
import { KIND_LABEL, nodeIdProblem, outgoing, stateReferences } from "@/lib/workflow";
import { countFor } from "@/lib/workflowErrors";
import { Badge } from "../Badge";
import Button from "../Button";
import { CheckboxField, FieldErrors as ErrorList, SelectField, TextAreaField, TextField, type Option } from "../fields";
import JsonEditor, { type BlockerFn } from "./JsonEditor";

export const TEMPLATE_HINT = "Use {{.input.field}} for a value from the run input and {{.state.node_id.field}} for an earlier node's output.";

type Props = {
  def: WorkflowDefinition;
  node: WorkflowNode;
  agents: Agent[];
  /** Errors for this node, keyed by the rest of the path ("task"); "" is the node itself. */
  errors: FieldErrors;
  /** Errors by edge index, to flag the edges listed here. */
  edgeErrors: Record<number, FieldErrors>;
  onChange: (patch: Partial<WorkflowNode>) => void;
  onRename: (id: string) => void;
  onDelete: () => void;
  onAddEdge: (to: string) => void;
  onSelectEdge: (index: number) => void;
  onMoveEdge: (index: number, delta: -1 | 1) => void;
  onBlocker: BlockerFn;
};

const unset = (v: string) => (v === "" ? undefined : v);

export default function NodePanel({ def, node, agents, errors, edgeErrors, onChange, onRename, onDelete, onAddEdge, onSelectEdge, onMoveEdge, onBlocker }: Props) {
  const [idText, setIdText] = useState(node.id);
  const [renameNote, setRenameNote] = useState<string | null>(null);
  const idProblem = nodeIdProblem(def, node.id, idText);

  const others = def.nodes.filter((n) => n.id !== node.id);
  const targets: Option[] = [{ value: "", label: "None" }, ...others.filter((n) => n.kind !== "start").map((n) => ({ value: n.id, label: n.id }))];
  const edges = outgoing(def, node.id);

  const agentId = node.agent_id ?? agents.find((a) => a.name.toLowerCase() === node.agent_name?.toLowerCase())?.id ?? "";
  const agentOptions: Option[] = [
    { value: "", label: "Choose an agent…" },
    ...agents.map((a) => ({ value: a.id, label: a.name })),
    ...(agentId && !agents.some((a) => a.id === agentId) ? [{ value: agentId, label: "Unknown agent" }] : []),
  ];

  return (
    <section aria-label="Node settings" className="space-y-4">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <h2 className="text-base font-semibold">{KIND_LABEL[node.kind] ?? node.kind} node</h2>
          <Badge tone="zinc">{node.kind}</Badge>
        </div>
        <Button size="sm" variant="ghost" onClick={onDelete}>
          Delete node
        </Button>
      </div>
      <ErrorList errors={errors[""] ?? []} />

      <TextField
        label="Node id"
        mono
        value={idText}
        maxLength={40}
        hint="Other nodes, conditions and templates refer to this node by its id."
        errors={[...(idProblem ? [idProblem] : []), ...errorsFor(errors, "id")]}
        onChange={(v) => {
          setIdText(v);
          if (v === node.id || nodeIdProblem(def, node.id, v)) return;
          const refs = stateReferences(def, node.id);
          onRename(v);
          setRenameNote(refs > 0 ? `Updated ${refs} ${refs === 1 ? "reference" : "references"} in conditions and templates to use ${v}.` : null);
        }}
      />
      {renameNote ? (
        <p role="status" className="-mt-2 text-xs text-zinc-500">
          {renameNote}
        </p>
      ) : null}

      {node.kind === "start" ? <p className="text-sm text-zinc-500">The run begins here. Its output is the run input.</p> : null}
      {node.kind === "end" ? <p className="text-sm text-zinc-500">The run finishes successfully when it reaches this node.</p> : null}

      {node.kind === "agent" ? (
        <>
          <SelectField
            label="Agent"
            required
            value={agentId}
            options={agentOptions}
            hint={node.agent_name && !node.agent_id ? `The template names this agent “${node.agent_name}”.` : undefined}
            errors={[...errorsFor(errors, "agent_id"), ...errorsFor(errors, "agent_name")]}
            onChange={(v) => onChange({ agent_id: unset(v), agent_name: undefined })}
          />
          <TextAreaField
            label="Task"
            required
            rows={8}
            value={node.task ?? ""}
            hint={TEMPLATE_HINT}
            placeholder="What should this agent do?"
            errors={errorsFor(errors, "task")}
            onChange={(v) => onChange({ task: v })}
          />
          <JsonEditor
            label="Output schema"
            hint="The JSON Schema the agent's result must satisfy. Its fields become available as {{.state.node_id.field}}."
            rows={8}
            value={node.output_schema}
            onChange={(v) => onChange({ output_schema: v })}
            blockerKey={`node:${node.id}:output_schema`}
            onBlocker={onBlocker}
            errors={errorsFor(errors, "output_schema")}
          />
          <fieldset>
            <legend className="mb-1 text-sm font-medium">Receives output from</legend>
            <p className="mb-2 text-xs text-zinc-500">The listed nodes' results are added to the task as context. The start node stands for the run input.</p>
            <div className="space-y-1.5">
              {others
                .filter((n) => n.kind !== "end")
                .map((n) => (
                  <CheckboxField
                    key={n.id}
                    label={n.id}
                    hint={n.kind === "start" ? "Run input" : undefined}
                    checked={node.input_from?.includes(n.id) ?? false}
                    onChange={(on) => {
                      const current = node.input_from ?? [];
                      const next = on ? [...current, n.id] : current.filter((x) => x !== n.id);
                      onChange({ input_from: next.length > 0 ? next : undefined });
                    }}
                  />
                ))}
            </div>
            <ErrorList errors={errorsFor(errors, "input_from")} />
          </fieldset>
          <SelectField
            label="If this node fails, go to"
            value={node.on_failure ?? ""}
            options={targets}
            hint="Without a target the run fails with the node's reason."
            errors={errorsFor(errors, "on_failure")}
            onChange={(v) => onChange({ on_failure: unset(v) })}
          />
        </>
      ) : null}

      {node.kind === "approval" ? (
        <>
          <TextAreaField
            label="Message"
            rows={4}
            value={node.message ?? ""}
            hint={`What the approver sees. ${TEMPLATE_HINT}`}
            placeholder="Deploy this release?"
            errors={errorsFor(errors, "message")}
            onChange={(v) => onChange({ message: v })}
          />
          <SelectField
            label="If rejected, go to"
            value={node.on_reject ?? ""}
            options={[{ value: "", label: "Fail the run" }, ...targets.slice(1)]}
            errors={errorsFor(errors, "on_reject")}
            onChange={(v) => onChange({ on_reject: unset(v) })}
          />
        </>
      ) : null}

      {node.kind !== "end" ? (
        <div>
          <h3 className="mb-1 text-sm font-medium">Outgoing edges</h3>
          <p className="mb-2 text-xs text-zinc-500">Edges are checked in this order and the first one whose condition holds is taken.</p>
          {edges.length === 0 ? (
            <p className="text-sm text-zinc-500">No outgoing edges yet.</p>
          ) : (
            <ol className="space-y-1.5">
              {edges.map(({ edge, index }, position) => {
                const bad = countFor(edgeErrors[index]);
                return (
                  <li key={index} className="flex items-center gap-1.5 rounded-md border border-zinc-200 px-2 py-1.5 text-sm dark:border-zinc-800">
                    <span className="w-5 shrink-0 text-center text-xs font-medium tabular-nums text-zinc-500" aria-label={`Order ${position + 1}`}>
                      {position + 1}
                    </span>
                    <button
                      type="button"
                      onClick={() => onSelectEdge(index)}
                      aria-label={`Edit edge to ${edge.to}`}
                      className="min-w-0 flex-1 text-left hover:underline"
                    >
                      <span className="font-mono">{edge.to}</span>
                      <span className="block truncate text-xs text-zinc-500">{describeCondition(edge.when)}</span>
                    </button>
                    {bad > 0 ? <Badge tone="red">{bad}</Badge> : null}
                    <Button size="sm" variant="ghost" aria-label={`Move edge to ${edge.to} earlier`} disabled={position === 0} onClick={() => onMoveEdge(index, -1)}>
                      ↑
                    </Button>
                    <Button size="sm" variant="ghost" aria-label={`Move edge to ${edge.to} later`} disabled={position === edges.length - 1} onClick={() => onMoveEdge(index, 1)}>
                      ↓
                    </Button>
                  </li>
                );
              })}
            </ol>
          )}
          <label className="mt-2 block text-xs text-zinc-500">
            <span className="sr-only">Add edge to</span>
            <select
              aria-label="Add edge to"
              value=""
              onChange={(e) => e.target.value && onAddEdge(e.target.value)}
              className="w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
            >
              <option value="">Add edge to…</option>
              {def.nodes
                .filter((n) => n.kind !== "start")
                .map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.id}
                  </option>
                ))}
            </select>
          </label>
        </div>
      ) : null}
    </section>
  );
}
