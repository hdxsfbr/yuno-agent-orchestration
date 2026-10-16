import { Link } from "react-router";
import type { Skill } from "@/lib/api";
import { CheckboxField, FieldErrors } from "../fields";
import { Loading, QueryError } from "../QueryState";

type Props = {
  skills: { data?: Skill[]; isLoading: boolean; error: unknown; refetch: () => void };
  selected: string[];
  onChange: (names: string[]) => void;
  errors?: string[];
};

/** The skills an agent has: every skill that exists, ticked when the agent uses it. */
export default function SkillPicker({ skills, selected, onChange, errors = [] }: Props) {
  const available = skills.data ?? [];
  const known = new Set(available.map((s) => s.name));
  // A name the agent still lists although the skill is gone stays visible so it can be removed.
  const missing = selected.filter((n) => !known.has(n));
  const toggle = (name: string, on: boolean) => onChange(on ? [...selected, name] : selected.filter((n) => n !== name));

  return (
    <fieldset>
      <legend className="mb-1 text-sm font-medium">Skills</legend>
      <p className="mb-2 text-xs text-zinc-500">
        Instructions the agent can follow. Write and edit them on the{" "}
        <Link to="/skills" className="underline">
          Skills page
        </Link>
        .
      </p>
      {skills.isLoading ? <Loading label="Loading skills…" /> : null}
      {skills.error ? <QueryError what="skills" error={skills.error} onRetry={skills.refetch} /> : null}
      {skills.data && available.length === 0 && missing.length === 0 ? <p className="text-sm text-zinc-500">No skills yet.</p> : null}
      <div className="space-y-2">
        {available.map((s) => (
          <CheckboxField key={s.id} label={s.name} hint={s.description || undefined} checked={selected.includes(s.name)} onChange={(on) => toggle(s.name, on)} />
        ))}
        {missing.map((n) => (
          <CheckboxField key={n} label={n} hint="This skill no longer exists." checked onChange={(on) => toggle(n, on)} />
        ))}
      </div>
      <FieldErrors errors={errors} />
    </fieldset>
  );
}
