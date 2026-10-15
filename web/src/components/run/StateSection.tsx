import JsonView from "../JsonView";

/** The run's typed state: each node's latest output under the node's id. Follows run.status events. */
export default function StateSection({ state }: { state: Record<string, unknown> | null | undefined }) {
  if (!state || Object.keys(state).length === 0) return <p className="text-sm text-zinc-500">Empty until a node finishes.</p>;
  return <JsonView label="Run state" value={state} defaultOpen />;
}
