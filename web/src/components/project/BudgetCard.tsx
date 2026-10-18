import type { Project } from "@/lib/api";
import { formatDateTime, formatUsd } from "@/lib/format";
import { BUDGET_TEXT, BUDGET_TONE_TITLE, budgetTone } from "@/lib/project";
import { Card } from "../Card";

function Window({ label, spent, cap, until, note }: { label: string; spent: number; cap: number | undefined; until?: string; note: boolean }) {
  const tone = budgetTone(spent, cap);
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-zinc-500">{label}</dt>
      <dd className="mt-0.5">
        <p className={`text-lg tabular-nums ${BUDGET_TEXT[tone]}`} title={BUDGET_TONE_TITLE[tone]}>
          {cap == null ? formatUsd(spent) : `${formatUsd(spent)} / ${formatUsd(cap)}`}
        </p>
        {cap != null && until ? <p className="text-xs text-zinc-500">{`resets at ${formatDateTime(until)}`}</p> : null}
        {cap == null && note ? <p className="text-xs text-zinc-500">No cap set.</p> : null}
      </dd>
    </div>
  );
}

/** Today's and this month's spend over the caps, with when each window ends; the amounts alone when there is no cap. */
export default function BudgetCard({ project }: { project: Project }) {
  const day = project.max_cost_usd_per_day;
  const month = project.max_cost_usd_per_month;
  const noCaps = day == null && month == null;
  return (
    <section aria-labelledby="project-budget">
      <h2 id="project-budget" className="mb-2 text-base font-semibold">
        Budget
      </h2>
      <Card>
        <dl className="grid gap-4 sm:grid-cols-2">
          <Window label="Today" spent={project.spend?.day_usd ?? 0} cap={day} until={project.spend?.day_until} note={!noCaps} />
          <Window label="This month" spent={project.spend?.month_usd ?? 0} cap={month} until={project.spend?.month_until} note={!noCaps} />
        </dl>
        {noCaps ? <p className="mt-3 text-sm text-zinc-500">No cap set.</p> : null}
      </Card>
    </section>
  );
}
