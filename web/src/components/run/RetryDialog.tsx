import { useState, type FormEvent } from "react";
import { useMutation } from "@tanstack/react-query";
import { ApiError, retryRun, type Run } from "@/lib/api";
import { errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { RETRY_LIMIT_FIELDS, exhaustedLimits, retryLimits, startingDraft, validateRetry, type RetryDraft } from "@/lib/runRetry";
import Button from "../Button";
import { NumberField } from "../fields";
import { Modal } from "../Modal";
import { InlineError, errorText } from "../QueryState";

type Props = {
  run: Run;
  /** The refusal that opened the dialog: 409 `limits_exhausted`, whose details name each limit that is used up. */
  refusal: ApiError;
  /** The run already deployed a release or created a GitLab request, which a retry can repeat. */
  repeatsSideEffects?: boolean;
  onClose: () => void;
  /** Called with the run the API returns once it is queued again. */
  onRetried: (run: Run) => void;
};

/**
 * A run that stopped on a limit cannot be retried as it is. The dialog lists the limits that
 * are used up, with fields starting from the run's limits, and retries with the raised ones.
 */
export default function RetryDialog({ run, refusal, repeatsSideEffects = false, onClose, onRetried }: Props) {
  const [exhausted, setExhausted] = useState(() => exhaustedLimits(refusal.details));
  // Where the fields start decides what counts as raised, so it is fixed once.
  const [start] = useState<RetryDraft>(() => startingDraft(run.limits, exhausted));
  const [draft, setDraft] = useState<RetryDraft>(start);
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  const retry = useMutation({
    mutationFn: (limits: ReturnType<typeof retryLimits>) => retryRun(run.id, limits),
    onSuccess: (queued) => {
      onRetried(queued);
      onClose();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409 && err.code === "limits_exhausted") {
        setExhausted(exhaustedLimits(err.details));
        setFormError(err.message);
      } else if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (retry.isPending) return;
    setServerErrors({});
    setFormError(null);
    const found = validateRetry(draft, start, new Set(Object.keys(exhausted)));
    setClientErrors(found);
    if (Object.keys(found).length > 0) return;
    retry.mutate(retryLimits(run.limits, draft));
  };

  const labelOf = (key: string) => RETRY_LIMIT_FIELDS.find((f) => f.key === key)?.label ?? key;
  const errors = (key: string) => [...errorsFor(clientErrors, key), ...errorsFor(serverErrors, key), ...errorsFor(serverErrors, `limits.${key}`)];
  const used = Object.entries(exhausted);
  // The loop visits of the step the run stopped at have a field only when they are what is used up.
  const fields = RETRY_LIMIT_FIELDS.filter((spec) => spec.key !== "max_loop_visits" || spec.key in exhausted);

  return (
    <Modal title="Retry run" onClose={retry.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} noValidate className="space-y-4">
        {used.length > 0 ? (
          <div>
            <p className="text-sm">The run stopped because a limit was used up.</p>
            <ul aria-label="Limits used up" className="mt-1.5 list-disc space-y-0.5 pl-5 text-sm">
              {used.map(([key, messages]) => (
                <li key={key}>
                  {labelOf(key)}: {messages.join("; ")}
                </li>
              ))}
            </ul>
          </div>
        ) : null}
        <p className="text-sm text-zinc-600 dark:text-zinc-400">Raise the limits that are used up to retry from where the run stopped. A field left empty keeps its current value.</p>
        <div className={`grid gap-4 ${fields.length > 3 ? "sm:grid-cols-2" : "sm:grid-cols-3"}`}>
          {fields.map((spec) => (
            <NumberField
              key={spec.key}
              label={spec.label}
              required={spec.key in exhausted}
              value={draft[spec.key]}
              placeholder="Unchanged"
              onChange={(v) => setDraft((d) => ({ ...d, [spec.key]: v }))}
              errors={errors(spec.key)}
            />
          ))}
        </div>
        {repeatsSideEffects ? (
          <p className="text-sm text-zinc-600 dark:text-zinc-400">
            This run already deployed a release or created a merge request through the GitLab API. Retrying repeats them if the agents reach those steps again.
          </p>
        ) : null}
        {formError ? <InlineError>{formError}</InlineError> : null}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={retry.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={retry.isPending}>
            {retry.isPending ? "Retrying…" : "Retry"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
