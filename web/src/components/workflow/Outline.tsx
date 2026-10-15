import type { WorkflowDefinition } from "@/lib/api";
import { describeCondition } from "@/lib/condition";
import { KIND_LABEL, outgoing } from "@/lib/workflow";
import { countFor, type WorkflowErrors } from "@/lib/workflowErrors";
import { Badge } from "../Badge";
import type { Selection } from "./selection";

type Props = {
  def: WorkflowDefinition;
  selected: Selection | null;
  errors: WorkflowErrors;
  onSelect: (s: Selection) => void;
};

const row = (active: boolean) =>
  `flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 ${active ? "bg-zinc-100 ring-1 ring-zinc-400 dark:bg-zinc-800 dark:ring-zinc-600" : ""}`;

/** Every node and edge as a list: the keyboard-friendly twin of the canvas. */
export default function Outline({ def, selected, errors, onSelect }: Props) {
  return (
    <section aria-label="Outline" className="grid gap-4 sm:grid-cols-2">
      <div>
        <h3 className="mb-1 text-sm font-medium">Nodes</h3>
        <ul>
          {def.nodes.map((n, i) => {
            const bad = countFor(errors.nodes[i]);
            const active = selected?.type === "node" && selected.id === n.id;
            return (
              <li key={`${n.id}:${i}`}>
                <button type="button" className={row(active)} aria-pressed={active} aria-label={`Node ${n.id}`} onClick={() => onSelect({ type: "node", id: n.id })}>
                  <span className="font-mono">{n.id}</span>
                  <span className="text-xs text-zinc-500">{KIND_LABEL[n.kind] ?? n.kind}</span>
                  {bad > 0 ? <Badge tone="red">{bad}</Badge> : null}
                </button>
              </li>
            );
          })}
        </ul>
      </div>
      <div>
        <h3 className="mb-1 text-sm font-medium">Edges</h3>
        {def.edges.length === 0 ? <p className="px-2 text-sm text-zinc-500">No edges yet.</p> : null}
        <ul>
          {def.edges.map((e, i) => {
            const siblings = outgoing(def, e.from);
            const order = siblings.findIndex((s) => s.index === i) + 1;
            const bad = countFor(errors.edges[i]);
            const active = selected?.type === "edge" && selected.index === i;
            return (
              <li key={i}>
                <button type="button" className={row(active)} aria-pressed={active} aria-label={`Edge ${e.from} to ${e.to}${siblings.length > 1 ? `, order ${order}` : ""}`} onClick={() => onSelect({ type: "edge", index: i })}>
                  <span className="font-mono">
                    {e.from} → {e.to}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-xs text-zinc-500">
                    {siblings.length > 1 ? `#${order} · ` : ""}
                    {describeCondition(e.when)}
                  </span>
                  {bad > 0 ? <Badge tone="red">{bad}</Badge> : null}
                </button>
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}
