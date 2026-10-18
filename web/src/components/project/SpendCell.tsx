import type { Project } from "@/lib/api";
import { formatUsd } from "@/lib/format";
import { BUDGET_TEXT, BUDGET_TONE_TITLE, budgetTone } from "@/lib/project";

function SpendLine({ window, spent, cap }: { window: string; spent: number; cap: number | undefined }) {
  const tone = budgetTone(spent, cap);
  const amounts = cap == null ? formatUsd(spent) : `${formatUsd(spent)} / ${formatUsd(cap)}`;
  return (
    <p className={BUDGET_TEXT[tone]} title={BUDGET_TONE_TITLE[tone]}>
      {`${amounts} ${window}`}
    </p>
  );
}

/** What the project spent today and this month, over its caps when it has them. */
export default function SpendCell({ project }: { project: Project }) {
  const spend = project.spend;
  if (!spend) return <span className="text-zinc-500">—</span>;
  return (
    <div className="space-y-0.5 whitespace-nowrap text-xs tabular-nums">
      <SpendLine window="today" spent={spend.day_usd} cap={project.max_cost_usd_per_day} />
      <SpendLine window="this month" spent={spend.month_usd} cap={project.max_cost_usd_per_month} />
    </div>
  );
}
