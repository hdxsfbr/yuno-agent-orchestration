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

describe("the messages of a fork and its join (DESIGN 5.26 rule 2)", () => {
  // The workflow: 0 start, 1 coder, 2 panel (fork), 3 security, 4 performance, 5 merge (join), 6 end.
  const fork = [
    { path: "nodes[2].join", message: "required for fork nodes" },
    { path: "nodes[2]", message: "a fork needs at least two outgoing edges" },
    { path: "nodes[5].checkout", message: "unknown branch 'audit'" },
    { path: "nodes[5].checkout", message: "the workflow does not work on a project" },
    { path: "nodes[3].agent_id", message: "agent 'a2' is used in two branches of 'panel' ('security' and 'performance'); branches run at once and share one workspace per agent" },
    { path: "nodes[3].input_from[0]", message: "'performance' is in another branch of 'panel'" },
    { path: "nodes[3].on_failure", message: "leaves the branch of 'panel' without reaching its join 'merge'" },
    { path: "edges[4].to", message: "a branch of 'panel' cannot start at its join" },
    { path: "edges[3].to", message: "duplicate branch of 'panel'" },
  ];

  it("files the join and the checkout under the node, keeping each message as the API wrote it", () => {
    const g = groupErrors(fork);
    expect(g.nodes[2]).toEqual({ join: ["required for fork nodes"], "": ["a fork needs at least two outgoing edges"] });
    expect(g.nodes[5]).toEqual({ checkout: ["unknown branch 'audit'", "the workflow does not work on a project"] });
    expect(countFor(g.nodes[5])).toBe(2);
  });

  it("files the other messages of the rule under the node or edge they name", () => {
    const g = groupErrors(fork);
    expect(g.nodes[3]).toMatchObject({
      agent_id: [expect.stringContaining("used in two branches of 'panel'")],
      "input_from.0": ["'performance' is in another branch of 'panel'"],
      on_failure: ["leaves the branch of 'panel' without reaching its join 'merge'"],
    });
    expect(g.edges[4]).toEqual({ to: ["a branch of 'panel' cannot start at its join"] });
    expect(g.edges[3]).toEqual({ to: ["duplicate branch of 'panel'"] });
  });

  it("lists every message in the API's spelling, each pointing at its node or edge", () => {
    const g = groupErrors(fork);
    expect(g.list.map((p) => p.path)).toEqual([
      "nodes[2].join",
      "nodes[2]",
      "nodes[5].checkout",
      "nodes[5].checkout",
      "nodes[3].agent_id",
      "nodes[3].input_from[0]",
      "nodes[3].on_failure",
      "edges[4].to",
      "edges[3].to",
    ]);
    expect(g.list[0]).toEqual({ path: "nodes[2].join", message: "required for fork nodes", target: { type: "node", index: 2 } });
    expect(g.list[2].target).toEqual({ type: "node", index: 5 });
    expect(g.list[7].target).toEqual({ type: "edge", index: 4 });
  });

  it("reads the string form for the join and the checkout too", () => {
    const g = groupErrors(["nodes[2].join: 'x' is not a join node", "nodes[5].checkout: only join nodes have one"]);
    expect(g.nodes[2]).toEqual({ join: ["'x' is not a join node"] });
    expect(g.nodes[5]).toEqual({ checkout: ["only join nodes have one"] });
  });

  it("takes the messages of a node away once it is edited", () => {
    const after = clearFor(groupErrors(fork), { type: "node", index: 2 });
    expect(after.nodes[2]).toBeUndefined();
    expect(after.nodes[5]).toBeDefined();
    expect(after.list.some((p) => p.path === "nodes[2].join")).toBe(false);
  });
});
