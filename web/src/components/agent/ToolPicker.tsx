import { useId } from "react";
import type { Tool, ToolPolicy } from "@/lib/api";
import { defaultPolicy, errorsFor, type FieldErrors, type FormState } from "@/lib/agentForm";
import { humanize } from "@/lib/format";
import { Badge, type Tone } from "../Badge";
import { FieldErrors as ErrorList, control } from "../fields";
import { Loading, QueryError } from "../QueryState";

const riskTone: Record<Tool["risk"], Tone> = { read_only: "green", computational: "blue", side_effecting: "amber" };

const policyOptions: { value: ToolPolicy; label: string }[] = [
  { value: "auto", label: "Auto" },
  { value: "require_approval", label: "Require approval" },
  { value: "blocked", label: "Blocked" },
];

type Props = {
  tools: { data?: Tool[]; isLoading: boolean; error: unknown; refetch: () => void };
  selected: FormState["tools"];
  onChange: (tools: FormState["tools"]) => void;
  errors: FieldErrors;
};

export default function ToolPicker({ tools, selected, onChange, errors }: Props) {
  const registry = tools.data ?? [];
  const known = new Set(registry.map((t) => t.name));
  const platform = registry.filter((t) => t.source === "platform");
  const builtin = registry.filter((t) => t.source === "openclaw");
  // Allowed tools the registry does not list (removed since, or the registry did not load).
  const other = Object.keys(selected)
    .filter((n) => !known.has(n))
    .sort();

  const toggle = (name: string, on: boolean) => {
    const next = { ...selected };
    if (on) next[name] = { policy: null };
    else delete next[name];
    onChange(next);
  };
  const setPolicy = (name: string, policy: ToolPolicy) => onChange({ ...selected, [name]: { policy } });

  return (
    <div className="space-y-5">
      {tools.isLoading ? <Loading label="Loading tools…" /> : null}
      {tools.error ? <QueryError what="the tool list" error={tools.error} onRetry={tools.refetch} /> : null}
      <ErrorList errors={errorsFor(errors, "tool_allowlist")} />
      <Group title="Platform tools" tools={platform} selected={selected} errors={errors} onToggle={toggle} onPolicy={setPolicy} />
      <Group title="OpenClaw built-ins" tools={builtin} selected={selected} errors={errors} onToggle={toggle} onPolicy={setPolicy} />
      {other.length > 0 ? (
        <Group
          title="Other allowed tools"
          tools={other.map((name) => ({ name, description: "", risk: "computational", hits: "none", timeout_seconds: 0, source: "platform" }) as Tool)}
          selected={selected}
          errors={errors}
          onToggle={toggle}
          onPolicy={setPolicy}
          hideMeta
        />
      ) : null}
    </div>
  );
}

type GroupProps = {
  title: string;
  tools: Tool[];
  selected: FormState["tools"];
  errors: FieldErrors;
  onToggle: (name: string, on: boolean) => void;
  onPolicy: (name: string, policy: ToolPolicy) => void;
  hideMeta?: boolean;
};

function Group({ title, tools, selected, errors, onToggle, onPolicy, hideMeta }: GroupProps) {
  if (tools.length === 0) return null;
  const allowed = tools.filter((t) => selected[t.name]).length;
  return (
    <fieldset>
      <legend className="mb-2 flex w-full items-baseline justify-between text-sm font-medium">
        <span>{title}</span>
        <span className="text-xs font-normal text-zinc-500">
          {allowed} of {tools.length} allowed
        </span>
      </legend>
      <ul className="divide-y divide-zinc-200 rounded-md border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
        {tools.map((t) => (
          <ToolRow
            key={t.name}
            tool={t}
            allowed={!!selected[t.name]}
            policy={selected[t.name]?.policy ?? null}
            errors={errorsFor(errors, `interaction_rules.${t.name}`)}
            onToggle={(on) => onToggle(t.name, on)}
            onPolicy={(p) => onPolicy(t.name, p)}
            hideMeta={hideMeta}
          />
        ))}
      </ul>
    </fieldset>
  );
}

type RowProps = {
  tool: Tool;
  allowed: boolean;
  policy: ToolPolicy | null;
  errors: string[];
  onToggle: (on: boolean) => void;
  onPolicy: (p: ToolPolicy) => void;
  hideMeta?: boolean;
};

function ToolRow({ tool, allowed, policy, errors, onToggle, onPolicy, hideMeta }: RowProps) {
  const id = useId();
  const effective = policy ?? defaultPolicy(tool);
  const unattended = allowed && effective === "auto" && tool.risk === "side_effecting";
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-2 p-3">
      <input
        id={id}
        type="checkbox"
        checked={allowed}
        onChange={(e) => onToggle(e.target.checked)}
        className="mt-1 size-4 rounded border-zinc-300 accent-zinc-900 dark:accent-zinc-100"
      />
      <div className="min-w-0 flex-1">
        <label htmlFor={id} className="flex flex-wrap items-center gap-2 text-sm">
          <span className="font-mono font-medium">{tool.name}</span>
          {hideMeta ? null : (
            <>
              <Badge tone={riskTone[tool.risk]}>{humanize(tool.risk)}</Badge>
              <Badge>{tool.source === "platform" ? "platform" : "OpenClaw"}</Badge>
            </>
          )}
        </label>
        {tool.description ? <p className="mt-0.5 text-xs text-zinc-500">{tool.description}</p> : null}
        {unattended ? <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">This tool changes things outside the platform and will run without asking for approval.</p> : null}
        <ErrorList errors={errors} />
      </div>
      {allowed ? (
        <select
          aria-label={`Policy for ${tool.name}`}
          value={effective}
          onChange={(e) => onPolicy(e.target.value as ToolPolicy)}
          className={control(errors.length > 0, "!w-auto")}
        >
          {policyOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : null}
    </li>
  );
}
