import { addError, parseFieldErrors, type FieldErrors } from "./agentForm";

// A 422 from validate or save names its problems by path: `nodes[1].task`, `edges[2].to`,
// `limits.max_steps`, `nodes` (no start node). The builder shows each next to the element
// the path names, so the details are regrouped by node index and edge index.

export type Problem = {
  /** The path as the API wrote it, for the list. */
  path: string;
  message: string;
  target?: { type: "node" | "edge"; index: number };
};

export type WorkflowErrors = {
  /** Members of one node, keyed by the rest of the path ("task"); "" is the node itself. */
  nodes: Record<number, FieldErrors>;
  edges: Record<number, FieldErrors>;
  /** Everything else, keyed by the full dotted path ("limits.max_steps", "name"). */
  other: FieldErrors;
  list: Problem[];
};

export const noErrors = (): WorkflowErrors => ({ nodes: {}, edges: {}, other: {}, list: [] });

const ELEMENT = /^(nodes|edges)\.(\d+)(?:\.(.*))?$/;

/** `nodes.2.task` back to the `nodes[2].task` spelling of the API. */
const display = (key: string) => key.replace(/\.(\d+)/g, "[$1]");

export function groupErrors(details: unknown): WorkflowErrors {
  const out = noErrors();
  for (const [key, messages] of Object.entries(parseFieldErrors(details))) {
    const m = ELEMENT.exec(key);
    for (const message of messages) {
      if (m) {
        const index = Number(m[2]);
        const bucket = m[1] === "nodes" ? out.nodes : out.edges;
        addError((bucket[index] ??= {}), m[3] ?? "", message);
        out.list.push({ path: display(key), message, target: { type: m[1] === "nodes" ? "node" : "edge", index } });
      } else {
        addError(out.other, key, message);
        out.list.push({ path: display(key), message });
      }
    }
  }
  return out;
}

export function countFor(errors: FieldErrors | undefined): number {
  return errors ? Object.values(errors).reduce((n, m) => n + m.length, 0) : 0;
}

/** What an edit touched: one node, one edge, or the members of the definition under `prefix`. */
export type Touch = { type: "node" | "edge"; index: number } | { type: "other"; prefix: string };

/** The errors without the ones an edit has just made stale (edit a task, its message goes away). */
export function clearFor(errors: WorkflowErrors, touch: Touch): WorkflowErrors {
  if (touch.type === "other") {
    const other = Object.fromEntries(Object.entries(errors.other).filter(([k]) => !(k === touch.prefix || k.startsWith(`${touch.prefix}.`))));
    return { ...errors, other, list: errors.list.filter((p) => p.target || !(p.path === touch.prefix || p.path.startsWith(`${touch.prefix}.`))) };
  }
  const bucket = touch.type === "node" ? errors.nodes : errors.edges;
  if (!(touch.index in bucket)) return errors;
  const rest = { ...bucket };
  delete rest[touch.index];
  return {
    ...errors,
    ...(touch.type === "node" ? { nodes: rest } : { edges: rest }),
    list: errors.list.filter((p) => !(p.target?.type === touch.type && p.target.index === touch.index)),
  };
}
