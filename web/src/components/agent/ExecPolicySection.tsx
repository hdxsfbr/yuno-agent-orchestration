import { EXEC_ASK, EXEC_SECURITY, type ExecAsk, type ExecSecurity } from "@/lib/api";
import { errorsFor, type FieldErrors, type FormState } from "@/lib/agentForm";
import { SelectField, toOptions } from "../fields";
import TagInput from "../TagInput";

type Props = { form: FormState; set: (patch: Partial<FormState>) => void; errors: FieldErrors };

export default function ExecPolicySection({ form, set, errors }: Props) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-zinc-500">
        Applies to commands that run on the host. Commands in the agent sandbox never need approval.
      </p>
      <div className="grid gap-4 sm:grid-cols-3">
        <SelectField
          label="Security"
          hint="deny blocks every command, allowlist permits matching ones, full permits all."
          value={form.execSecurity}
          onChange={(v) => set({ execSecurity: v as ExecSecurity })}
          options={toOptions(EXEC_SECURITY)}
          errors={errorsFor(errors, "exec_policy.security")}
        />
        <SelectField
          label="Ask"
          hint="When to ask a person before a command runs."
          value={form.execAsk}
          onChange={(v) => set({ execAsk: v as ExecAsk })}
          options={toOptions(EXEC_ASK)}
          errors={errorsFor(errors, "exec_policy.ask")}
        />
        <SelectField
          label="Ask fallback"
          hint="What happens when nobody answers in time."
          value={form.execAskFallback}
          onChange={(v) => set({ execAskFallback: v as ExecSecurity })}
          options={toOptions(EXEC_SECURITY)}
          errors={errorsFor(errors, "exec_policy.ask_fallback")}
        />
      </div>
      <TagInput
        label="Allowlist"
        hint="Command patterns that run without asking, for example git status or npm test*."
        values={form.execAllowlist}
        onChange={(execAllowlist) => set({ execAllowlist })}
        placeholder="Command pattern"
        errors={errorsFor(errors, "exec_policy.allowlist")}
        mono
      />
    </div>
  );
}
