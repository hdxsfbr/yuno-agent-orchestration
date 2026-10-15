import type { WorkflowDefinition } from "@/lib/api";
import type { Problem } from "@/lib/workflowErrors";
import type { Selection } from "./selection";

type Props = {
  problems: Problem[];
  def: WorkflowDefinition;
  onSelect: (s: Selection) => void;
};

/** The server's validation messages, each pointing at the node or edge it names. */
export default function Problems({ problems, def, onSelect }: Props) {
  if (problems.length === 0) return null;
  return (
    <section aria-label="Problems" className="rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-900 dark:bg-red-950/40">
      <h2 className="mb-2 text-sm font-semibold text-red-800 dark:text-red-300">
        {problems.length} {problems.length === 1 ? "problem" : "problems"} to fix
      </h2>
      <ul className="space-y-1 text-sm text-red-800 dark:text-red-300">
        {problems.map((p, i) => {
          const sel: Selection | undefined =
            p.target?.type === "node" && def.nodes[p.target.index]
              ? { type: "node", id: def.nodes[p.target.index].id }
              : p.target?.type === "edge" && def.edges[p.target.index]
                ? { type: "edge", index: p.target.index }
                : undefined;
          const body = (
            <>
              <span className="font-mono text-xs">{p.path}</span>
              <span className="ml-2">{p.message}</span>
            </>
          );
          return (
            <li key={i}>
              {sel ? (
                <button type="button" onClick={() => onSelect(sel)} className="text-left hover:underline">
                  {body}
                </button>
              ) : (
                body
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
