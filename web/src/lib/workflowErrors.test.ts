import { describe, expect, it } from "vitest";
import { clearFor, countFor, groupErrors } from "./workflowErrors";

const details = [
  { path: "nodes[1].task", message: "required for agent nodes" },
  { path: "nodes[1].agent_id", message: "required for agent nodes" },
  { path: "edges[2].to", message: "unknown node 'reviewr'" },
  { path: "nodes[3]", message: "'gate' has no outgoing edge" },
  { path: "nodes", message: "exactly one start node required, found 0" },
  { path: "limits.max_steps", message: "must be at least 1" },
];

describe("groupErrors", () => {
  it("files each message under the node or edge its path names", () => {
    const g = groupErrors(details);
    expect(g.nodes[1]).toEqual({ task: ["required for agent nodes"], agent_id: ["required for agent nodes"] });
    expect(g.edges[2]).toEqual({ to: ["unknown node 'reviewr'"] });
    expect(g.nodes[3]).toEqual({ "": ["'gate' has no outgoing edge"] });
  });

  it("keeps everything else under its full path and lists every message in the API's spelling", () => {
    const g = groupErrors(details);
    expect(g.other).toEqual({ nodes: ["exactly one start node required, found 0"], "limits.max_steps": ["must be at least 1"] });
    expect(g.list.map((p) => p.path)).toEqual(["nodes[1].task", "nodes[1].agent_id", "edges[2].to", "nodes[3]", "nodes", "limits.max_steps"]);
    expect(g.list[0].target).toEqual({ type: "node", index: 1 });
    expect(g.list[2].target).toEqual({ type: "edge", index: 2 });
    expect(g.list[4].target).toBeUndefined();
  });

  it("reads the string form 'path: message' too", () => {
    const g = groupErrors(["nodes[0].id: duplicate 'a'", "edges[1].from: unknown node 'x'"]);
    expect(g.nodes[0]).toEqual({ id: ["duplicate 'a'"] });
    expect(g.edges[1]).toEqual({ from: ["unknown node 'x'"] });
  });

  it("handles a response with no details", () => {
    const g = groupErrors(undefined);
    expect(g.list).toEqual([]);
    expect(countFor(g.nodes[0])).toBe(0);
  });
});

describe("clearFor", () => {
  it("drops the messages of the element an edit touched and nothing else", () => {
    const g = groupErrors(details);
    const afterNode = clearFor(g, { type: "node", index: 1 });
    expect(afterNode.nodes[1]).toBeUndefined();
    expect(afterNode.edges[2]).toBeDefined();
    expect(afterNode.list.map((p) => p.path)).toEqual(["edges[2].to", "nodes[3]", "nodes", "limits.max_steps"]);
    const afterLimits = clearFor(g, { type: "other", prefix: "limits" });
    expect(afterLimits.other).toEqual({ nodes: ["exactly one start node required, found 0"] });
    expect(afterLimits.list.some((p) => p.path === "limits.max_steps")).toBe(false);
    expect(afterLimits.list.some((p) => p.path === "nodes")).toBe(true);
  });
});
