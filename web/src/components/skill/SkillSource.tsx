import { Badge, type Tone } from "../Badge";
import type { Skill, SkillSource } from "@/lib/api";

const labels: Record<SkillSource, string> = { library: "Library", git: "Git", upload: "Upload", user: "Custom" };
const tones: Record<SkillSource, Tone> = { library: "blue", git: "orange", upload: "amber", user: "zinc" };

export const sourceOf = (s: Skill): SkillSource => s.source ?? "user";
export const sourceLabel = (s: SkillSource) => labels[s];
export const SOURCES = Object.keys(labels) as SkillSource[];

export function SourceBadge({ skill }: { skill: Skill }) {
  const src = sourceOf(skill);
  return <Badge tone={tones[src]}>{labels[src]}</Badge>;
}

/** Where an imported skill came from and under which licence; nothing for a skill made on the page. */
export function SourceLink({ skill }: { skill: Skill }) {
  const src = sourceOf(skill);
  if (src === "user") return <span>Made on this page.</span>;
  const safe = skill.source_url && /^https?:\/\//.test(skill.source_url) ? skill.source_url : undefined;
  return (
    <span>
      {labels[src]} skill
      {skill.source_url ? (
        <>
          {" from "}
          {safe ? (
            <a href={safe} target="_blank" rel="noreferrer noopener" className="underline">
              {skill.source_url}
            </a>
          ) : (
            skill.source_url
          )}
        </>
      ) : null}
      {skill.source_ref ? <span className="font-mono text-xs"> @ {skill.source_ref.slice(0, 12)}</span> : null}
      {skill.license ? <span> · Licence {skill.license}</span> : null}
    </span>
  );
}
