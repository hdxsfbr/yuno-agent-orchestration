import type { Condition, NodeKind, Position, WorkflowDefinition, WorkflowEdge, WorkflowNode } from "./api";

// Pure edits on a workflow definition. Every function returns a new definition and spreads
// the objects it changes, so members the builder does not know about (and the `ui` key the
// backend keeps) survive any edit.

/** DESIGN 5.6 rule 1. */
export const NODE_ID = /^[a-z0-9_]{1,40}$/;

export const KIND_LABEL: Record<NodeKind, string> = {
  start: "Start",
  agent: "Agent",
  approval: "Approval",
  fork: "Fork",
  join: "Join",
  end: "End",
};

export function newDefinition(): WorkflowDefinition {
  return {
    input_schema: { type: "object", properties: { task: { type: "string" } }, required: ["task"] },
    nodes: [
      { id: "start", kind: "start" },
      { id: "end", kind: "end" },
    ],
    edges: [{ from: "start", to: "end" }],
    limits: {},
  };
}

/** Applies a patch; a member set to undefined is removed rather than serialised as null. */
function patched<T extends object>(obj: T, patch: Partial<T>): T {
  const next = { ...obj } as Record<string, unknown>;
  for (const [k, v] of Object.entries(patch)) {
    if (v === undefined) delete next[k];
    else next[k] = v;
  }
  return next as T;
}

/**
 * A shipped template names its agents (`agent_name`) instead of pointing at them, so it
 * cannot run as it is. A copy made from it keeps no such node, whatever its template key.
 */
export function isShippedTemplate(w: { template_key?: string; definition: WorkflowDefinition }): boolean {
  return !!w.template_key && w.definition.nodes.some((n) => n.kind === "agent" && !n.agent_id && !!n.agent_name);
}

export function nodeIds(def: WorkflowDefinition): string[] {
  return def.nodes.map((n) => n.id);
}

export function findNode(def: WorkflowDefinition, id: string): WorkflowNode | undefined {
  return def.nodes.find((n) => n.id === id);
}

/** `agent`, then `agent_2`, `agent_3`, … : the first id of that kind not taken. */
export function nextNodeId(def: WorkflowDefinition, kind: NodeKind): string {
  const taken = new Set(nodeIds(def));
  if (!taken.has(kind)) return kind;
  for (let n = 2; ; n++) if (!taken.has(`${kind}_${n}`)) return `${kind}_${n}`;
}

export function addNode(def: WorkflowDefinition, kind: NodeKind, position?: Position): { def: WorkflowDefinition; id: string } {
  const id = nextNodeId(def, kind);
  const node: WorkflowNode = kind === "agent" ? { id, kind, task: "" } : kind === "fork" ? { id, kind, join: "" } : { id, kind };
  const positions = position ? { ...def.ui?.positions, [id]: position } : def.ui?.positions;
  return {
    id,
    def: { ...def, nodes: [...def.nodes, node], ...(positions ? { ui: { ...def.ui, positions } } : {}) },
  };
}

export function updateNode(def: WorkflowDefinition, id: string, patch: Partial<WorkflowNode>): WorkflowDefinition {
  return { ...def, nodes: def.nodes.map((n) => (n.id === id ? patched(n, patch) : n)) };
}

/** Deletes a node, its edges and every reference other nodes hold to it (a fork's join, a join's checkout). */
export function removeNode(def: WorkflowDefinition, id: string): WorkflowDefinition {
  const positions = def.ui?.positions;
  const rest = positions ? Object.fromEntries(Object.entries(positions).filter(([k]) => k !== id)) : undefined;
  return {
    ...def,
    nodes: def.nodes
      .filter((n) => n.id !== id)
      .map((n) => {
        const patch: Partial<WorkflowNode> = {};
        if (n.input_from?.includes(id)) {
          const left = n.input_from.filter((x) => x !== id);
          patch.input_from = left.length > 0 ? left : undefined;
        }
        if (n.on_reject === id) patch.on_reject = undefined;
        if (n.on_failure === id) patch.on_failure = undefined;
        if (n.join === id) patch.join = n.kind === "fork" ? "" : undefined;
        if (n.checkout === id) patch.checkout = undefined;
        return Object.keys(patch).length > 0 ? patched(n, patch) : n;
      }),
    edges: def.edges.filter((e) => e.from !== id && e.to !== id),
    ...(rest ? { ui: { ...def.ui, positions: rest } } : {}),
  };
}

/** Whether a condition path points into a node's output: "/<id>" or "/<id>/…". */
function pathInto(path: string | undefined, id: string): boolean {
  return path === `/${id}` || !!path?.startsWith(`/${id}/`);
}

/** `.state.<id>` in a task or message template, not a longer id that starts the same way. */
function stateRef(id: string): RegExp {
  return new RegExp(`\\.state\\.${id.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![A-Za-z0-9_])`, "g");
}

function renameInCondition(c: Condition, from: string, to: string): Condition {
  const next = { ...c };
  if (c.path !== undefined && pathInto(c.path, from)) next.path = `/${to}${c.path.slice(from.length + 1)}`;
  if (c.all) next.all = c.all.map((x) => renameInCondition(x, from, to));
  if (c.any) next.any = c.any.map((x) => renameInCondition(x, from, to));
  return next;
}

/**
 * How many condition paths and `.state.<id>` template references read a node's output:
 * what renaming the node rewrites besides the plain links to it.
 */
export function stateReferences(def: WorkflowDefinition, id: string): number {
  let n = 0;
  const count = (c: Condition | undefined) => {
    if (!c) return;
    if (pathInto(c.path, id)) n++;
    c.all?.forEach(count);
    c.any?.forEach(count);
  };
  def.edges.forEach((e) => count(e.when));
  for (const node of def.nodes) {
    for (const text of [node.task, node.message]) n += text?.match(stateRef(id))?.length ?? 0;
  }
  return n;
}

/**
 * Renames a node and everything that points at it: edges, input_from, on_reject,
 * on_failure, a fork's join, a join's checkout, positions, condition paths into its output and `.state.<id>` references in
 * task and message templates. The caller checks the new id first.
 */
export function renameNode(def: WorkflowDefinition, from: string, to: string): WorkflowDefinition {
  if (from === to) return def;
  const swap = (v: string | undefined) => (v === from ? to : v);
  const ref = stateRef(from);
  const positions = def.ui?.positions;
  const moved =
    positions && from in positions
      ? Object.fromEntries(Object.entries(positions).map(([k, v]) => [k === from ? to : k, v]))
      : positions;
  return {
    ...def,
    nodes: def.nodes.map((n) => {
      const next = { ...n, id: swap(n.id) as string };
      if (n.input_from) next.input_from = n.input_from.map((x) => (x === from ? to : x));
      if (n.on_reject) next.on_reject = swap(n.on_reject);
      if (n.on_failure) next.on_failure = swap(n.on_failure);
      if (n.join) next.join = swap(n.join);
      if (n.checkout) next.checkout = swap(n.checkout);
      if (n.task) next.task = n.task.replace(ref, `.state.${to}`);
      if (n.message) next.message = n.message.replace(ref, `.state.${to}`);
      return next;
    }),
    edges: def.edges.map((e) => {
      const next = { ...e, from: swap(e.from) as string, to: swap(e.to) as string };
      if (e.when) next.when = renameInCondition(e.when, from, to);
      return next;
    }),
    ...(moved ? { ui: { ...def.ui, positions: moved } } : {}),
  };
}

/** Why `id` cannot be used as the new id of the node currently called `current`, if so. */
export function nodeIdProblem(def: WorkflowDefinition, current: string, id: string): string | null {
  if (id === current) return null;
  if (!NODE_ID.test(id)) return "Use 1 to 40 lowercase letters, digits or underscores";
  if (def.nodes.some((n) => n.id === id)) return `Another node is already called ${id}`;
  return null;
}

export function addEdge(def: WorkflowDefinition, from: string, to: string): { def: WorkflowDefinition; index: number } {
  return { def: { ...def, edges: [...def.edges, { from, to }] }, index: def.edges.length };
}

/** An edge edit that takes a head away from a fork also takes it away from the checkout of the join that fork ends at. */
export function updateEdge(def: WorkflowDefinition, index: number, patch: Partial<WorkflowEdge>): WorkflowDefinition {
  return dropLostCheckouts(def, { ...def, edges: def.edges.map((e, i) => (i === index ? patched(e, patch) : e)) });
}

export function removeEdge(def: WorkflowDefinition, index: number): WorkflowDefinition {
  return dropLostCheckouts(def, { ...def, edges: def.edges.filter((_, i) => i !== index) });
}

/** The heads of every fork that ends at a join (DESIGN 5.26 rule 1), each once. */
function headsAtJoin(def: WorkflowDefinition, joinId: string): string[] {
  return [...new Set(forksJoiningAt(def, joinId).flatMap((f) => forkHeads(def, f.id)))];
}

/**
 * `after` is `before` with an edge edited. A join whose checkout was a head of its fork in `before` and is none in `after`
 * (the edge to it was removed, or now leads or starts elsewhere) loses the checkout, which would name a branch that no longer exists.
 */
function dropLostCheckouts(before: WorkflowDefinition, after: WorkflowDefinition): WorkflowDefinition {
  let changed = false;
  const nodes = after.nodes.map((n) => {
    if (n.kind !== "join" || !n.checkout) return n;
    if (!headsAtJoin(before, n.id).includes(n.checkout) || headsAtJoin(after, n.id).includes(n.checkout)) return n;
    changed = true;
    return patched(n, { checkout: undefined });
  });
  return changed ? { ...after, nodes } : after;
}

/** The edges leaving a node in evaluation order (DESIGN 5.6 rule 4), with their array index. */
export function outgoing(def: WorkflowDefinition, from: string): { edge: WorkflowEdge; index: number }[] {
  const out: { edge: WorkflowEdge; index: number }[] = [];
  def.edges.forEach((edge, index) => {
    if (edge.from === from) out.push({ edge, index });
  });
  return out;
}

/** The fork nodes that name `joinId` as the join their branches end at (DESIGN 5.26 rule 1). */
export function forksJoiningAt(def: WorkflowDefinition, joinId: string): WorkflowNode[] {
  return def.nodes.filter((n) => n.kind === "fork" && n.join === joinId);
}

/** The heads of a fork's branches: the targets of its outgoing edges in edge order, each once (DESIGN 5.26 rule 2). */
export function forkHeads(def: WorkflowDefinition, forkId: string): string[] {
  const heads: string[] = [];
  for (const { edge } of outgoing(def, forkId)) if (!heads.includes(edge.to)) heads.push(edge.to);
  return heads;
}

/**
 * Moves an edge one place earlier (-1) or later (+1) among the edges of its source node,
 * leaving every other edge where it is. Returns the edge's new index.
 */
export function moveEdge(def: WorkflowDefinition, index: number, delta: -1 | 1): { def: WorkflowDefinition; index: number } {
  const edge = def.edges[index];
  if (!edge) return { def, index };
  const siblings = outgoing(def, edge.from);
  const at = siblings.findIndex((s) => s.index === index);
  const other = siblings[at + delta];
  if (!other) return { def, index };
  const edges = def.edges.slice();
  edges[index] = def.edges[other.index];
  edges[other.index] = edge;
  return { def: { ...def, edges }, index: other.index };
}

export function setPositions(def: WorkflowDefinition, positions: Record<string, Position>): WorkflowDefinition {
  return { ...def, ui: { ...def.ui, positions } };
}

export function setPosition(def: WorkflowDefinition, id: string, position: Position): WorkflowDefinition {
  return setPositions(def, { ...def.ui?.positions, [id]: position });
}
