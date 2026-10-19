import { memo } from "react";
import { Handle, Position as Side, type Node, type NodeProps } from "@xyflow/react";
import type { NodeKind } from "@/lib/api";
import { KIND_LABEL } from "@/lib/workflow";

export type StepData = {
  id: string;
  kind: NodeKind;
  /** The agent's name, the start of an approval message, or a fork's join as `→ <join>`. */
  subtitle?: string;
  /** How many validation messages name this node. */
  problems: number;
};
export type StepNodeType = Node<StepData, "step">;

const accent: Record<NodeKind, string> = {
  start: "border-l-emerald-500",
  agent: "border-l-blue-500",
  approval: "border-l-amber-500",
  fork: "border-l-violet-500 dark:border-l-violet-400",
  join: "border-l-violet-500 dark:border-l-violet-400",
  end: "border-l-zinc-500",
};

function StepNode({ data, selected }: NodeProps<StepNodeType>) {
  const bad = data.problems > 0;
  return (
    <div
      className={`w-52 rounded-lg border border-l-4 bg-white px-3 py-2 shadow-sm dark:bg-zinc-900 ${accent[data.kind]} ${
        bad ? "border-red-500" : selected ? "border-zinc-900 ring-2 ring-zinc-400 dark:border-zinc-100" : "border-zinc-300 dark:border-zinc-700"
      }`}
    >
      {data.kind === "start" ? null : <Handle type="target" position={Side.Left} />}
      <div className="flex items-center justify-between gap-2">
        <span className="text-[10px] font-medium uppercase tracking-wide text-zinc-500">{KIND_LABEL[data.kind] ?? data.kind}</span>
        {bad ? (
          <span className="rounded-full bg-red-600 px-1.5 text-[10px] font-medium text-white" title="Validation problems on this node">
            {data.problems}
          </span>
        ) : null}
      </div>
      <div className="truncate font-mono text-sm font-semibold">{data.id}</div>
      {data.subtitle ? <div className="truncate text-xs text-zinc-500">{data.subtitle}</div> : null}
      {data.kind === "end" ? null : <Handle type="source" position={Side.Right} />}
    </div>
  );
}

export default memo(StepNode);
