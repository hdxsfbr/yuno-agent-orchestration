import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { getPlan } from "@/lib/api";
import { shipRunRef } from "@/lib/plan";

/** "Feature <feature>, wave <n>" for a ship run, linking to the project's Plans section; "Plan wave <n>" until the plan is read (DESIGN 5.29 rule 9). */
export default function PlanWaveLink({ run }: { run: { trigger: string; trigger_ref?: string; project_id?: string } }) {
  const ref = shipRunRef(run);
  const plan = useQuery({ queryKey: ["plans", "detail", ref?.planId ?? ""], queryFn: () => getPlan(ref!.planId), enabled: !!ref, staleTime: 30_000 });
  if (!ref) return null;
  const projectId = plan.data?.plan.project_id ?? run.project_id;
  const text = plan.data ? `Feature ${plan.data.plan.feature}, wave ${ref.wave}` : `Plan wave ${ref.wave}`;
  return projectId ? (
    <Link to={`/projects/${encodeURIComponent(projectId)}#project-plans`} className="underline" title="Open the plan">
      {text}
    </Link>
  ) : (
    <>{text}</>
  );
}
