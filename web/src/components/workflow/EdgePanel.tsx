import type { Condition, WorkflowDefinition, WorkflowEdge } from "@/lib/api";
import { errorsFor, type FieldErrors } from "@/lib/agentForm";
import { outgoing } from "@/lib/workflow";
import Button from "../Button";
import { FieldErrors as ErrorList, SelectField, toOptions } from "../fields";
import ConditionEditor from "./ConditionEditor";
import type { BlockerFn } from "./JsonEditor";

type Props = {
  def: WorkflowDefinition;
  edge: WorkflowEdge;
  index: number;
  /** Errors for this edge, keyed by the rest of the path ("to", "when.path"); "" is the edge itself. */
  errors: FieldErrors;
  onChange: (patch: Partial<WorkflowEdge>) => void;
  onMove: (delta: -1 | 1) => void;
  onDelete: () => void;
  onSelectNode: (id: string) => void;
  onBlocker: BlockerFn;
};

export default function EdgePanel({ def, edge, index, errors, onChange, onMove, onDelete, onSelectNode, onBlocker }: Props) {
  const siblings = outgoing(def, edge.from);
  const position = siblings.findIndex((s) => s.index === index);
  const ids = def.nodes.map((n) => n.id);
  const known = (id: string) => (ids.includes(id) ? ids : [id, ...ids]);

  return (
    <section aria-label="Edge settings" className="space-y-4">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-base font-semibold">
          Edge <span className="font-mono">{edge.from}</span> → <span className="font-mono">{edge.to}</span>
        </h2>
        <Button size="sm" variant="ghost" onClick={onDelete}>
          Delete edge
        </Button>
      </div>
      <ErrorList errors={errors[""] ?? []} />

      <div className="grid grid-cols-2 gap-3">
        <SelectField label="From" value={edge.from} options={toOptions(known(edge.from))} errors={errorsFor(errors, "from")} onChange={(from) => onChange({ from })} />
        <SelectField label="To" value={edge.to} options={toOptions(known(edge.to))} errors={errorsFor(errors, "to")} onChange={(to) => onChange({ to })} />
      </div>

      <div>
        <div className="flex items-center justify-between gap-2">
          <div className="text-sm">
            <span className="font-medium">Order</span>{" "}
            <span className="tabular-nums" data-testid="edge-order">
              {position + 1} of {siblings.length}
            </span>
          </div>
          <div className="flex gap-1.5">
            <Button size="sm" aria-label="Move earlier" disabled={position <= 0} onClick={() => onMove(-1)}>
              Earlier
            </Button>
            <Button size="sm" aria-label="Move later" disabled={position < 0 || position >= siblings.length - 1} onClick={() => onMove(1)}>
              Later
            </Button>
          </div>
        </div>
        <p className="mt-1 text-xs text-zinc-500">
          The edges leaving <span className="font-mono">{edge.from}</span> are checked in this order; the first whose condition holds is taken.
        </p>
      </div>

      <ConditionEditor
        value={edge.when}
        onChange={(when: Condition | undefined) => onChange({ when })}
        errors={errors}
        blockerKey={`edge:${index}:when`}
        onBlocker={onBlocker}
      />

      <button type="button" onClick={() => onSelectNode(edge.from)} className="text-sm text-zinc-500 underline">
        Edit node {edge.from}
      </button>
    </section>
  );
}
