import type { CIStatus, MergeRequestState } from "@/lib/api";
import { CI_LABEL, STATE_LABEL } from "@/lib/mergeRequest";
import { Badge, type Tone } from "./Badge";

const STATE_TONE: Record<MergeRequestState, Tone> = { open: "blue", merged: "green", closed: "zinc" };
const CI_TONE: Record<CIStatus, Tone> = { none: "zinc", pending: "amber", passed: "green", failed: "red" };

/** Open, Merged or Closed. */
export function MergeRequestStateBadge({ state }: { state: MergeRequestState }) {
  return <Badge tone={STATE_TONE[state] ?? "zinc"}>{STATE_LABEL[state] ?? state}</Badge>;
}

/** The pipeline of the request: None, Pending, Passed or Failed; `labelled` says "CI: " first, for where no column header does. */
export function CIStatusBadge({ status, labelled = false }: { status: CIStatus; labelled?: boolean }) {
  return (
    <Badge tone={CI_TONE[status] ?? "zinc"} title="Pipeline status">
      {labelled ? "CI: " : ""}
      {CI_LABEL[status] ?? status}
    </Badge>
  );
}
