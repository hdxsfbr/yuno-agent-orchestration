import { describe, expect, it } from "vitest";
import type { WorkflowDefinition } from "./api";
import {
  addEdge,
  KIND_LABEL,
  addNode,
  forkHeads,
  forksJoiningAt,
  isShippedTemplate,
  moveEdge,
  newDefinition,
  nextNodeId,
  nodeIdProblem,
  outgoing,
  removeEdge,
  removeNode,
  renameNode,
  stateReferences,
  updateEdge,
  updateNode,
} from "./workflow";
import { COLUMN_WIDTH, autoLayout, nextPosition, withPositions } from "./workflowLayout";

// start -> coder -> reviewer; reviewer -> coder (changes) | gate (approved); gate -> end
const pipeline = (): WorkflowDefinition => ({
  nodes: [
    { id: "start", kind: "start" },
    { id: "coder", kind: "agent", agent_id: "a1", task: "write", input_from: ["start", "reviewer"], on_failure: "end" },
    { id: "reviewer", kind: "agent", agent_id: "a2", task: "review", input_from: ["coder"] },
    { id: "gate", kind: "approval", message: "ok?", on_reject: "coder" },
    { id: "end", kind: "end" },
  ],
  edges: [
    { from: "start", to: "coder" },
    { from: "coder", to: "reviewer" },
    { from: "reviewer", to: "coder", when: { path: "/reviewer/verdict", op: "eq", value: "changes_requested" } },
    { from: "reviewer", to: "gate", when: { path: "/reviewer/verdict", op: "eq", value: "approved" } },
    { from: "gate", to: "end" },
  ],
  limits: { max_steps: 10 },
  ui: { positions: { coder: { x: 1, y: 2 } } },
});

describe("editing a definition", () => {
  it("keeps members the builder does not know about", () => {
    const def = { ...pipeline(), future_key: { a: 1 } } as WorkflowDefinition;
    const edited = updateNode(updateEdge(def, 2, { to: "gate" }), "coder", { task: "changed" });
    expect((edited as unknown as { future_key: unknown }).future_key).toEqual({ a: 1 });
    expect(edited.ui).toEqual(def.ui);
    expect(edited.limits).toEqual({ max_steps: 10 });
  });

  it("removes a member set to undefined instead of writing null", () => {
    const edited = updateNode(pipeline(), "coder", { on_failure: undefined });
    expect("on_failure" in edited.nodes[1]).toBe(false);
  });

  it("names new nodes after their kind and skips taken ids", () => {
    let def = newDefinition();
    expect(nextNodeId(def, "agent")).toBe("agent");
    def = addNode(def, "agent").def;
    expect(nextNodeId(def, "agent")).toBe("agent_2");
    expect(addNode(def, "agent").id).toBe("agent_2");
    expect(nextNodeId(def, "end")).toBe("end_2");
  });

  it("starts a new workflow as start to end", () => {
    const def = newDefinition();
    expect(def.nodes.map((n) => n.kind)).toEqual(["start", "end"]);
    expect(def.edges).toEqual([{ from: "start", to: "end" }]);
  });

  it("deleting a node removes its edges and every reference to it", () => {
    const def = removeNode(pipeline(), "coder");
    expect(def.nodes.map((n) => n.id)).toEqual(["start", "reviewer", "gate", "end"]);
    expect(def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["reviewer>gate", "gate>end"]);
    expect(def.nodes.find((n) => n.id === "gate")?.on_reject).toBeUndefined();
    expect("input_from" in (def.nodes.find((n) => n.id === "reviewer") ?? {})).toBe(false);
    expect(def.ui?.positions).toEqual({});
  });

  it("renaming a node updates edges, input_from, on_reject, on_failure and positions", () => {
    const def = renameNode(pipeline(), "coder", "writer");
    expect(def.nodes.map((n) => n.id)).toEqual(["start", "writer", "reviewer", "gate", "end"]);
    expect(def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>writer", "writer>reviewer", "reviewer>writer", "reviewer>gate", "gate>end"]);
    expect(def.nodes[3].on_reject).toBe("writer");
    expect(def.ui?.positions).toEqual({ writer: { x: 1, y: 2 } });
    const reviewer = renameNode(pipeline(), "reviewer", "critic");
    expect(reviewer.nodes[1].input_from).toEqual(["start", "critic"]);
    expect(reviewer.edges[2].when).toEqual({ path: "/critic/verdict", op: "eq", value: "changes_requested" });
  });

  it("renaming a node rewrites condition paths and .state references to its output, and nothing else", () => {
    const def: WorkflowDefinition = {
      nodes: [
        { id: "start", kind: "start" },
        { id: "coder", kind: "agent", task: "Fix {{with .state.coder}}{{.summary}}{{end}} per {{.state.reviewer.feedback}}; {{.state.coder_2.x}} {{.input.coder}}" },
        { id: "coder_2", kind: "agent", task: "Check {{.state.coder.files}}" },
        { id: "gate", kind: "approval", message: "Ship {{.state.coder.summary}} ({{.state.coder}})?" },
        { id: "end", kind: "end" },
      ],
      edges: [
        { from: "start", to: "coder" },
        { from: "coder", to: "coder_2", when: { path: "/coder", op: "exists" } },
        {
          from: "coder_2",
          to: "gate",
          when: { all: [{ path: "/coder/tests_passed", op: "eq", value: true }, { any: [{ path: "/coder_2/x", op: "exists" }, { path: "/coderx", op: "exists" }] }] },
        },
        { from: "gate", to: "end" },
      ],
    };
    expect(stateReferences(def, "coder")).toBe(6);
    const out = renameNode(def, "coder", "writer");
    expect(out.nodes[1].task).toBe("Fix {{with .state.writer}}{{.summary}}{{end}} per {{.state.reviewer.feedback}}; {{.state.coder_2.x}} {{.input.coder}}");
    expect(out.nodes[2].task).toBe("Check {{.state.writer.files}}");
    expect(out.nodes[3].message).toBe("Ship {{.state.writer.summary}} ({{.state.writer}})?");
    expect(out.edges[1].when).toEqual({ path: "/writer", op: "exists" });
    expect(out.edges[2].when).toEqual({
      all: [{ path: "/writer/tests_passed", op: "eq", value: true }, { any: [{ path: "/coder_2/x", op: "exists" }, { path: "/coderx", op: "exists" }] }],
    });
    expect("when" in out.edges[0]).toBe(false);
    expect(stateReferences(out, "coder")).toBe(0);
    expect(stateReferences(out, "writer")).toBe(6);
    expect(def.nodes[1].task).toContain(".state.coder}}"); // the input is not changed
  });

  it("checks a new node id against the pattern and the other nodes", () => {
    const def = pipeline();
    expect(nodeIdProblem(def, "coder", "coder")).toBeNull();
    expect(nodeIdProblem(def, "coder", "writer")).toBeNull();
    expect(nodeIdProblem(def, "coder", "Writer")).toMatch(/lowercase/);
    expect(nodeIdProblem(def, "coder", "")).toMatch(/lowercase/);
    expect(nodeIdProblem(def, "coder", "x".repeat(41))).toMatch(/lowercase/);
    expect(nodeIdProblem(def, "coder", "reviewer")).toMatch(/already called reviewer/);
  });

  it("adds and removes edges by index", () => {
    const added = addEdge(pipeline(), "gate", "coder");
    expect(added.index).toBe(5);
    expect(added.def.edges[5]).toEqual({ from: "gate", to: "coder" });
    expect(removeEdge(added.def, 5).edges).toHaveLength(5);
  });
});

describe("edge order", () => {
  it("lists a node's outgoing edges in definition order with their index", () => {
    expect(outgoing(pipeline(), "reviewer").map((o) => o.index)).toEqual([2, 3]);
  });

  it("moves an edge among its siblings and leaves every other edge in place", () => {
    const def = pipeline();
    const later = moveEdge(def, 2, 1);
    expect(later.index).toBe(3);
    expect(later.def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["start>coder", "coder>reviewer", "reviewer>gate", "reviewer>coder", "gate>end"]);
    const back = moveEdge(later.def, 3, -1);
    expect(back.def.edges).toEqual(def.edges);
  });

  it("moves across edges of other nodes that sit between the siblings", () => {
    const def: WorkflowDefinition = {
      nodes: [],
      edges: [
        { from: "a", to: "x" },
        { from: "b", to: "y" },
        { from: "a", to: "z" },
      ],
    };
    const r = moveEdge(def, 0, 1);
    expect(r.index).toBe(2);
    expect(r.def.edges.map((e) => `${e.from}>${e.to}`)).toEqual(["a>z", "b>y", "a>x"]);
  });

  it("does nothing past either end", () => {
    const def = pipeline();
    expect(moveEdge(def, 2, -1).def).toBe(def);
    expect(moveEdge(def, 3, 1).def).toBe(def);
    expect(moveEdge(def, 0, -1).def).toBe(def);
  });
});

// start -> coder -> panel (fork) -> security | performance -> merge (join) -> end
const panel = (): WorkflowDefinition => ({
  project: true,
  nodes: [
    { id: "start", kind: "start" },
    { id: "coder", kind: "agent", agent_id: "a1", task: "write" },
    { id: "panel", kind: "fork", join: "merge" },
    { id: "security", kind: "agent", agent_id: "a2", task: "review security" },
    { id: "performance", kind: "agent", agent_id: "a3", task: "review speed" },
    { id: "merge", kind: "join", checkout: "security" },
    { id: "end", kind: "end" },
  ],
  edges: [
    { from: "start", to: "coder" },
    { from: "coder", to: "panel" },
    { from: "panel", to: "performance" },
    { from: "panel", to: "security", when: { path: "/coder/risky", op: "eq", value: true } },
    { from: "security", to: "merge" },
    { from: "performance", to: "merge" },
    { from: "merge", to: "end" },
  ],
});

describe("fork and join nodes", () => {
  it("labels both kinds", () => {
    expect(KIND_LABEL.fork).toBe("Fork");
    expect(KIND_LABEL.join).toBe("Join");
  });

  it("adds a fork with an empty join and a join with nothing, named after their kind", () => {
    let { def, id } = addNode(newDefinition(), "fork");
    expect(id).toBe("fork");
    expect(def.nodes[def.nodes.length - 1]).toEqual({ id: "fork", kind: "fork", join: "" });
    ({ def, id } = addNode(def, "join"));
    expect(id).toBe("join");
    expect(def.nodes[def.nodes.length - 1]).toEqual({ id: "join", kind: "join" });
    expect(addNode(def, "fork").id).toBe("fork_2");
    expect(addNode(def, "join").id).toBe("join_2");
  });

  it("finds the fork that names a join and lists its heads in edge order, each once", () => {
    const def = panel();
    expect(forksJoiningAt(def, "merge").map((n) => n.id)).toEqual(["panel"]);
    expect(forksJoiningAt(def, "end")).toEqual([]);
    expect(forkHeads(def, "panel")).toEqual(["performance", "security"]);
    expect(forkHeads({ ...def, edges: [...def.edges, { from: "panel", to: "security" }] }, "panel")).toEqual(["performance", "security"]);
    expect(forkHeads(def, "merge")).toEqual(["end"]);
  });

  it("renaming the join or a head rewrites the fork's join and the join's checkout", () => {
    const joined = renameNode(panel(), "merge", "meet");
    expect(joined.nodes.find((n) => n.id === "panel")?.join).toBe("meet");
    expect(joined.nodes.find((n) => n.id === "meet")?.checkout).toBe("security");
    const head = renameNode(panel(), "security", "audit");
    expect(head.nodes.find((n) => n.id === "merge")?.checkout).toBe("audit");
    expect(head.nodes.find((n) => n.id === "panel")?.join).toBe("merge");
  });

  it("deleting the join empties the fork's join; deleting the checked-out head drops the checkout", () => {
    const noJoin = removeNode(panel(), "merge");
    expect(noJoin.nodes.find((n) => n.id === "panel")).toEqual({ id: "panel", kind: "fork", join: "" });
    const noHead = removeNode(panel(), "security");
    expect("checkout" in (noHead.nodes.find((n) => n.id === "merge") ?? {})).toBe(false);
    expect(noHead.nodes.find((n) => n.id === "panel")?.join).toBe("merge");
  });
});

describe("shipped templates", () => {
  it("is a template row only while an agent node names its agent without an id", () => {
    const named = { template_key: "dev", definition: { nodes: [{ id: "c", kind: "agent" as const, agent_name: "Coder" }], edges: [] } };
    expect(isShippedTemplate(named)).toBe(true);
    expect(isShippedTemplate({ ...named, template_key: undefined })).toBe(false);
    expect(isShippedTemplate({ ...named, definition: { nodes: [{ id: "c", kind: "agent", agent_id: "a1", agent_name: "Coder" }], edges: [] } })).toBe(false);
  });
});

describe("layout", () => {
  it("puts nodes in columns by distance from start", () => {
    const pos = autoLayout(pipeline());
    expect(pos.start.x).toBe(0);
    expect(pos.coder.x).toBeGreaterThan(pos.start.x);
    expect(pos.reviewer.x).toBeGreaterThan(pos.coder.x);
    expect(pos.gate.x).toBeGreaterThan(pos.reviewer.x);
    expect(pos.end.x).toBeGreaterThan(pos.gate.x);
    // the loop back from reviewer to coder does not move coder out of its column
    expect(pos.coder.x).toBe(COLUMN_WIDTH);
    expect(pos.reviewer.x).toBe(2 * COLUMN_WIDTH);
  });

  it("stacks siblings of one column and centres them on the tallest column", () => {
    const def: WorkflowDefinition = {
      nodes: [
        { id: "start", kind: "start" },
        { id: "a", kind: "agent" },
        { id: "b", kind: "agent" },
        { id: "end", kind: "end" },
      ],
      edges: [
        { from: "start", to: "a" },
        { from: "start", to: "b" },
        { from: "a", to: "end" },
        { from: "b", to: "end" },
      ],
    };
    const pos = autoLayout(def);
    expect(pos.a.x).toBe(pos.b.x);
    expect(pos.b.y).toBeGreaterThan(pos.a.y);
    expect(pos.start.y).toBeCloseTo((pos.a.y + pos.b.y) / 2);
  });

  it("puts nodes the start cannot reach in a column after the last", () => {
    const def = pipeline();
    def.nodes.push({ id: "orphan", kind: "agent" });
    const pos = autoLayout(def);
    expect(pos.orphan.x).toBeGreaterThan(pos.end.x);
  });

  it("keeps remembered positions and lays out only the nodes without one", () => {
    const def = pipeline();
    const pos = withPositions(def);
    expect(pos.coder).toEqual({ x: 1, y: 2 });
    expect(pos.reviewer).toEqual(autoLayout(def).reviewer);
  });

  it("places a new node to the right of the selected node without covering another", () => {
    const def = pipeline();
    const base = withPositions(def);
    const first = nextPosition(def, "start");
    expect(first.x).toBe(base.start.x + 280);
    expect(Object.values(base).some((p) => p.x === first.x && p.y === first.y)).toBe(false);
    expect(nextPosition(def).x).toBeGreaterThan(Math.max(...Object.values(base).map((p) => p.x)));
  });
});
