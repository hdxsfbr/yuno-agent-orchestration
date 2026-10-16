import Button from "../Button";
import { NumberField, control, FieldErrors as ErrorList } from "../fields";
import TagInput from "../TagInput";
import { errorsFor, type FieldErrors, type FormState, type PatternRow } from "@/lib/agentForm";

type Props = {
  form: FormState;
  set: (patch: Partial<FormState>) => void;
  errors: FieldErrors;
  toolNames: string[];
};

export default function GuardrailsSection({ form, set, errors, toolNames }: Props) {
  const updateRow = (i: number, patch: Partial<PatternRow>) =>
    set({ patterns: form.patterns.map((p, j) => (j === i ? { ...p, ...patch } : p)) });
  const removeRow = (i: number) => set({ patterns: form.patterns.filter((_, j) => j !== i) });

  return (
    <div className="space-y-5">
      <div className="grid gap-4 sm:grid-cols-2">
        <NumberField
          label="Cost per run (USD)"
          hint="A run stops when it spends more than this."
          value={form.maxCostPerRun}
          onChange={(v) => set({ maxCostPerRun: v })}
          errors={errorsFor(errors, "guardrails.max_cost_usd_per_run")}
          placeholder="No limit"
        />
        <NumberField
          label="Runs per hour"
          hint="New runs are refused above this rate."
          value={form.maxRunsPerHour}
          onChange={(v) => set({ maxRunsPerHour: v })}
          errors={errorsFor(errors, "guardrails.max_runs_per_hour")}
          placeholder="No limit"
        />
      </div>

      <TagInput
        label="Blocked tools"
        hint="The agent can never call these, even if they are allowed above."
        values={form.blockedTools}
        onChange={(blockedTools) => set({ blockedTools })}
        suggestions={toolNames}
        placeholder="Tool name"
        errors={errorsFor(errors, "guardrails.blocked_tools")}
        mono
      />

      <div>
        <div className="mb-1 text-sm font-medium">Blocked input patterns</div>
        <p className="mb-2 text-xs text-zinc-500">A tool call is refused when its input matches the regular expression.</p>
        <ErrorList errors={errors["guardrails.blocked_input_patterns"] ?? []} />
        {form.patterns.length > 0 ? (
          <ul className="space-y-3">
            {form.patterns.map((p, i) => {
              const base = `guardrails.blocked_input_patterns.${i}`;
              const nameErr = errorsFor(errors, `${base}.name`);
              const toolErr = errorsFor(errors, `${base}.tool`);
              // The API names the expression "pattern" in some messages and "regex" in the stored form.
              const regexErr = [...errorsFor(errors, `${base}.regex`), ...errorsFor(errors, `${base}.pattern`)];
              const rowErr = errors[base] ?? [];
              return (
                <li key={i} className="rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
                  <div className="grid gap-3 sm:grid-cols-[1fr_1fr_2fr_auto]">
                    <div>
                      <input
                        aria-label={`Pattern ${i + 1} name`}
                        placeholder="Name"
                        value={p.name}
                        onChange={(e) => updateRow(i, { name: e.target.value })}
                        aria-invalid={nameErr.length > 0 || undefined}
                        className={control(nameErr.length > 0)}
                      />
                      <ErrorList errors={nameErr} />
                    </div>
                    <div>
                      <input
                        aria-label={`Pattern ${i + 1} tool`}
                        placeholder="Tool"
                        list="tool-names"
                        value={p.tool}
                        onChange={(e) => updateRow(i, { tool: e.target.value })}
                        aria-invalid={toolErr.length > 0 || undefined}
                        className={control(toolErr.length > 0, "font-mono")}
                      />
                      <ErrorList errors={toolErr} />
                    </div>
                    <div>
                      <input
                        aria-label={`Pattern ${i + 1} regex`}
                        placeholder="Regular expression"
                        value={p.regex}
                        onChange={(e) => updateRow(i, { regex: e.target.value })}
                        aria-invalid={regexErr.length > 0 || undefined}
                        className={control(regexErr.length > 0, "font-mono")}
                      />
                      <ErrorList errors={regexErr} />
                    </div>
                    <Button variant="ghost" onClick={() => removeRow(i)} aria-label={`Remove pattern ${i + 1}`}>
                      Remove
                    </Button>
                  </div>
                  <ErrorList errors={rowErr} />
                </li>
              );
            })}
          </ul>
        ) : null}
        <datalist id="tool-names">
          {toolNames.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        <div className="mt-2">
          <Button onClick={() => set({ patterns: [...form.patterns, { name: "", tool: "", regex: "" }] })}>Add pattern</Button>
        </div>
      </div>
    </div>
  );
}
