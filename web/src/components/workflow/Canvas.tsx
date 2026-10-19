import { useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  Controls,
  MarkerType,
  ReactFlow,
  applyNodeChanges,
  type Edge,
  type NodeChange,
  type ReactFlowInstance,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { Position, WorkflowDefinition } from "@/lib/api";
import { describeCondition } from "@/lib/condition";
import { outgoing } from "@/lib/workflow";
import { withPositions } from "@/lib/workflowLayout";
import { countFor, type WorkflowErrors } from "@/lib/workflowErrors";
import type { Selection } from "./selection";
import StepNode, { type StepNodeType } from "./StepNode";

const nodeTypes = { step: StepNode };

type Props = {
  def: WorkflowDefinition;
  /** Agent names by id, for the subtitle of agent nodes. */
  agentNames: Record<string, string>;
  selected: Selection | null;
  errors: WorkflowErrors;
  /** Changes when the layout was reset, so the view fits the new positions. */
  layoutVersion: number;
  onSelect: (s: Selection | null) => void;
  onMove: (id: string, position: Position) => void;
  onConnect: (from: string, to: string) => void;
};

/**
 * The React Flow canvas. The definition is the single source of truth: nodes and edges are
 * derived from it on every change. The nodes live in local state as well because React Flow
 * writes measured sizes and in-progress drags into them; those are merged back in, and a
 * drag is stored in the definition when it ends.
 */
export default function Canvas({ def, agentNames, selected, errors, layoutVersion, onSelect, onMove, onConnect }: Props) {
  const derived = useMemo<StepNodeType[]>(() => {
    const positions = withPositions(def);
    return def.nodes.map((n, i) => ({
      id: n.id,
      type: "step",
      position: positions[n.id],
      selected: selected?.type === "node" && selected.id === n.id,
      deletable: false,
      data: {
        id: n.id,
        kind: n.kind,
        subtitle:
          n.kind === "agent"
            ? (n.agent_id && agentNames[n.agent_id]) || n.agent_name || (n.agent_id ? "Unknown agent" : "No agent chosen")
            : n.kind === "approval"
              ? n.message
              : n.kind === "fork" && n.join
                ? `→ ${n.join}`
                : undefined,
        problems: countFor(errors.nodes[i]),
      },
    }));
  }, [def, agentNames, selected, errors]);

  const [nodes, setNodes] = useState<StepNodeType[]>(derived);
  useEffect(() => {
    setNodes((prev) => {
      const old = new Map(prev.map((p) => [p.id, p]));
      return derived.map((d) => {
        const p = old.get(d.id);
        return p ? { ...d, measured: p.measured, width: p.width, height: p.height } : d;
      });
    });
  }, [derived]);

  const edges = useMemo<Edge[]>(() => {
    const known = new Set(def.nodes.map((n) => n.id));
    const out: Edge[] = [];
    def.edges.forEach((e, i) => {
      if (!known.has(e.from) || !known.has(e.to)) return;
      const siblings = outgoing(def, e.from);
      const order = siblings.findIndex((s) => s.index === i) + 1;
      const label = siblings.length > 1 ? `${order}. ${describeCondition(e.when)}` : e.when ? describeCondition(e.when) : undefined;
      const bad = countFor(errors.edges[i]) > 0;
      const active = selected?.type === "edge" && selected.index === i;
      out.push({
        id: `e${i}`,
        source: e.from,
        target: e.to,
        type: "smoothstep",
        label,
        labelStyle: { fontSize: 11 },
        markerEnd: { type: MarkerType.ArrowClosed },
        selected: active,
        deletable: false,
        style: { strokeWidth: active ? 3 : 1.5, ...(bad ? { stroke: "#dc2626" } : {}) },
      });
    });
    return out;
  }, [def, selected, errors]);

  const instance = useRef<ReactFlowInstance<StepNodeType, Edge> | null>(null);
  useEffect(() => {
    if (layoutVersion === 0) return;
    const frame = requestAnimationFrame(() => void instance.current?.fitView({ padding: 0.2, duration: 200 }));
    return () => cancelAnimationFrame(frame);
  }, [layoutVersion]);

  const onNodesChange = (changes: NodeChange<StepNodeType>[]) => {
    setNodes((ns) => applyNodeChanges(changes, ns));
  };

  return (
    <div className="h-[34rem] w-full rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-950" role="group" aria-label="Workflow canvas">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onInit={(i) => {
          instance.current = i;
        }}
        onNodeClick={(_, n) => onSelect({ type: "node", id: n.id })}
        onEdgeClick={(_, e) => onSelect({ type: "edge", index: Number(e.id.slice(1)) })}
        onPaneClick={() => onSelect(null)}
        onNodeDragStop={(_, n) => onMove(n.id, n.position)}
        onConnect={(c) => c.source && c.target && onConnect(c.source, c.target)}
        deleteKeyCode={null}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        minZoom={0.3}
        colorMode="system"
      >
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
