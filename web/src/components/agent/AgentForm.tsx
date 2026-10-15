import { useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router";
import { MODELS, THINKING_LEVELS, type Agent, type AgentInput, type Tool, type ThinkingLevel } from "@/lib/api";
import {
  LIMIT_FIELDS,
  SKILL_NAME,
  agentToForm,
  emptyForm,
  errorsFor,
  pruneBlankPatterns,
  toInput,
  unplacedErrors,
  validateForm,
  type FieldErrors,
  type FormState,
} from "@/lib/agentForm";
import Button, { buttonClass } from "../Button";
import { Card } from "../Card";
import { CheckboxField, NumberField, SelectField, TextAreaField, TextField, toOptions } from "../fields";
import TagInput from "../TagInput";
import ExecPolicySection from "./ExecPolicySection";
import GuardrailsSection from "./GuardrailsSection";
import ToolPicker from "./ToolPicker";

function Section({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <Card>
      <h2 className="text-base font-semibold">{title}</h2>
      {description ? <p className="mb-4 mt-0.5 text-sm text-zinc-500">{description}</p> : <div className="mb-4" />}
      {children}
    </Card>
  );
}

type Props = {
  /** The agent being edited; omit to create one. */
  agent?: Agent;
  tools: { data?: Tool[]; isLoading: boolean; error: unknown; refetch: () => void };
  saving: boolean;
  /** Field errors returned by the API (422). */
  serverErrors: FieldErrors;
  /** An error that belongs to the whole request rather than a field. */
  formError?: string | null;
  submitLabel: string;
  onSubmit: (input: AgentInput) => void;
};

export default function AgentForm({ agent, tools, saving, serverErrors, formError, submitLabel, onSubmit }: Props) {
  const [form, setForm] = useState<FormState>(() => (agent ? agentToForm(agent) : emptyForm()));
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [attempted, setAttempted] = useState(false);
  const summary = useRef<HTMLDivElement>(null);
  const set = (patch: Partial<FormState>) => setForm((f) => ({ ...f, ...patch }));

  // Client errors win until the next submit; server errors show for fields they name.
  const errors = useMemo<FieldErrors>(() => {
    const merged: FieldErrors = {};
    for (const src of [serverErrors, clientErrors]) {
      for (const [path, messages] of Object.entries(src)) {
        const list = (merged[path] ??= []);
        for (const m of messages) if (!list.includes(m)) list.push(m);
      }
    }
    return merged;
  }, [serverErrors, clientErrors]);

  // After a failed submit, keep the messages in step with what the user is fixing.
  useEffect(() => {
    if (attempted) setClientErrors(validateForm(form));
  }, [attempted, form]);

  const unplaced = unplacedErrors(errors);
  const count = Object.values(errors).reduce((n, m) => n + m.length, 0);
  const showSummary = count > 0 || !!formError;

  useEffect(() => {
    if (showSummary) summary.current?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [showSummary, serverErrors, formError]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const pruned = pruneBlankPatterns(form);
    if (pruned !== form) setForm(pruned);
    const found = validateForm(pruned);
    setClientErrors(found);
    setAttempted(true);
    if (Object.keys(found).length > 0) return;
    onSubmit(toInput(pruned));
  };

  const toolNames = (tools.data ?? []).map((t) => t.name);
  const modelOptions = toOptions(MODELS.includes(form.model as (typeof MODELS)[number]) ? MODELS : [form.model, ...MODELS]);

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      {showSummary ? (
        <div ref={summary} role="alert" className="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
          <p className="font-medium">{formError ?? "Some fields need attention."}</p>
          {unplaced.length > 0 ? (
            <ul className="mt-1 list-disc pl-5">
              {unplaced.map((m, i) => (
                <li key={i}>{m}</li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}

      <Section title="General">
        <div className="space-y-4">
          <TextField label="Name" required maxLength={80} value={form.name} onChange={(v) => set({ name: v })} errors={errorsFor(errors, "name")} />
          <TextField label="Role" hint="A short description of what this agent is for." value={form.role} onChange={(v) => set({ role: v })} errors={errorsFor(errors, "role")} />
          <TextAreaField label="System prompt" rows={8} value={form.systemPrompt} onChange={(v) => set({ systemPrompt: v })} errors={errorsFor(errors, "system_prompt")} />
        </div>
      </Section>

      <Section title="Model">
        <div className="grid gap-4 sm:grid-cols-2">
          <SelectField label="Model" required value={form.model} onChange={(v) => set({ model: v })} options={modelOptions} errors={errorsFor(errors, "model")} />
          <NumberField label="Max tokens" hint="Largest reply the model may write." value={form.maxTokens} onChange={(v) => set({ maxTokens: v })} errors={errorsFor(errors, "max_tokens")} placeholder="8192" />
          <SelectField label="Thinking" value={form.thinking} onChange={(v) => set({ thinking: v as ThinkingLevel })} options={toOptions(THINKING_LEVELS)} errors={errorsFor(errors, "thinking")} />
        </div>
      </Section>

      <Section title="Tools" description="Choose which tools the agent may call and whether each one needs a person's approval.">
        <ToolPicker tools={tools} selected={form.tools} onChange={(next) => set({ tools: next })} errors={errors} />
      </Section>

      <Section title="Limits" description="A run stops when it reaches any limit. Leave a field empty to use the platform default.">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {LIMIT_FIELDS.map((l) => (
            <NumberField
              key={l.key}
              label={l.label}
              value={form.limits[l.key]}
              onChange={(v) => set({ limits: { ...form.limits, [l.key]: v } })}
              placeholder={l.fallback}
              errors={errorsFor(errors, `limits.${l.key}`)}
            />
          ))}
        </div>
      </Section>

      <Section title="Guardrails">
        <GuardrailsSection form={form} set={set} errors={errors} toolNames={toolNames} />
      </Section>

      <Section title="Memory">
        <div className="space-y-4">
          <CheckboxField checked={form.memoryEnabled} onChange={(v) => set({ memoryEnabled: v })} label="Remember facts between runs" hint="Saved facts are recalled at the start of later runs." />
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <NumberField label="Max facts" value={form.maxFacts} onChange={(v) => set({ maxFacts: v })} errors={errorsFor(errors, "memory_policy.max_facts")} placeholder="Default" />
            <NumberField label="Fact lifetime (days)" value={form.factTtlDays} onChange={(v) => set({ factTtlDays: v })} errors={errorsFor(errors, "memory_policy.fact_ttl_days")} placeholder="Default" />
            <NumberField label="Conversation window (messages)" value={form.conversationWindow} onChange={(v) => set({ conversationWindow: v })} errors={errorsFor(errors, "memory_policy.conversation_window")} placeholder="Default" />
            <NumberField label="Conversation lifetime (days)" value={form.conversationTtlDays} onChange={(v) => set({ conversationTtlDays: v })} errors={errorsFor(errors, "memory_policy.conversation_ttl_days")} placeholder="Default" />
          </div>
        </div>
      </Section>

      <Section title="Exec policy">
        <ExecPolicySection form={form} set={set} errors={errors} />
      </Section>

      <Section title="Skills and channels">
        <div className="space-y-4">
          <TagInput
            label="Skills"
            hint="Names of skills to give the agent, in lowercase words separated by hyphens."
            values={form.skills}
            onChange={(skills) => set({ skills })}
            placeholder="skill-name"
            validate={(v) => (SKILL_NAME.test(v) ? null : "Use lowercase words separated by hyphens")}
            errors={errorsFor(errors, "skills")}
            mono
          />
          <CheckboxField checked={form.telegram} onChange={(v) => set({ telegram: v })} label="Telegram" hint="Let people talk to this agent over Telegram." />
          {errorsFor(errors, "channels").map((m, i) => (
            <p key={i} role="alert" className="text-xs text-red-600 dark:text-red-400">
              {m}
            </p>
          ))}
        </div>
      </Section>

      <div className="sticky bottom-3 flex justify-end gap-2 rounded-lg border border-zinc-200 bg-white/90 p-3 shadow-sm backdrop-blur dark:border-zinc-800 dark:bg-zinc-900/90">
        <Link to="/agents" className={buttonClass("secondary")}>
          Cancel
        </Link>
        <Button type="submit" variant="primary" disabled={saving}>
          {saving ? "Saving…" : submitLabel}
        </Button>
      </div>
    </form>
  );
}
