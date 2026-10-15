/** What the builder has open in its side panel. Edges have no id; they are identified by index. */
export type Selection = { type: "node"; id: string } | { type: "edge"; index: number };
