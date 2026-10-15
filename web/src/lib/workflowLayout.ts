import type { Position, WorkflowDefinition } from "./api";

export const COLUMN_WIDTH = 280;
export const ROW_HEIGHT = 130;

/**
 * A simple layered layout: a node's column is its breadth-first distance from `start`
 * (loops do not move a node back), nodes within a column keep definition order and are
 * centred on the tallest column. Nodes the start cannot reach go in a column of their own
 * after the last one.
 */
export function autoLayout(def: WorkflowDefinition): Record<string, Position> {
  const depth = new Map<string, number>();
  const start = def.nodes.find((n) => n.kind === "start");
  if (start) {
    depth.set(start.id, 0);
    const queue = [start.id];
    for (let head = 0; head < queue.length; head++) {
      const cur = queue[head];
      for (const e of def.edges) {
        if (e.from === cur && !depth.has(e.to) && def.nodes.some((n) => n.id === e.to)) {
          depth.set(e.to, (depth.get(cur) ?? 0) + 1);
          queue.push(e.to);
        }
      }
    }
  }
  const lastColumn = Math.max(-1, ...depth.values());
  const columns: string[][] = [];
  for (const n of def.nodes) {
    const d = depth.get(n.id) ?? lastColumn + 1;
    (columns[d] ??= []).push(n.id);
  }
  const tallest = Math.max(1, ...columns.map((c) => c?.length ?? 0));
  const out: Record<string, Position> = {};
  columns.forEach((ids, x) => {
    if (!ids) return;
    const offset = ((tallest - ids.length) * ROW_HEIGHT) / 2;
    ids.forEach((id, y) => {
      out[id] = { x: x * COLUMN_WIDTH, y: offset + y * ROW_HEIGHT };
    });
  });
  return out;
}

/** Remembered positions where they exist, the automatic layout for every other node. */
export function withPositions(def: WorkflowDefinition): Record<string, Position> {
  const saved = def.ui?.positions ?? {};
  const auto = autoLayout(def);
  const out: Record<string, Position> = {};
  for (const n of def.nodes) out[n.id] = saved[n.id] ?? auto[n.id] ?? { x: 0, y: 0 };
  return out;
}

/** Where a node added next goes: right of the selected node, else after the last column. */
export function nextPosition(def: WorkflowDefinition, near?: string): Position {
  const positions = withPositions(def);
  const anchor = near ? positions[near] : undefined;
  if (anchor) {
    const taken = Object.values(positions);
    let y = anchor.y;
    while (taken.some((p) => Math.abs(p.x - (anchor.x + COLUMN_WIDTH)) < 40 && Math.abs(p.y - y) < ROW_HEIGHT / 2)) y += ROW_HEIGHT;
    return { x: anchor.x + COLUMN_WIDTH, y };
  }
  const xs = Object.values(positions).map((p) => p.x);
  return { x: Math.max(0, ...xs) + COLUMN_WIDTH, y: 0 };
}
