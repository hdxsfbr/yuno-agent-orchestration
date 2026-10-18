import { useEffect, useId, type ReactNode } from "react";
import Button from "./Button";
import { InlineError } from "./QueryState";

export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const titleId = useId();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-zinc-950/50 p-4 pt-[12vh]"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="w-full max-w-lg rounded-lg border border-zinc-200 bg-white p-5 shadow-xl dark:border-zinc-800 dark:bg-zinc-900"
      >
        <h2 id={titleId} className="mb-3 text-base font-semibold">
          {title}
        </h2>
        {children}
      </div>
    </div>
  );
}

type ConfirmProps = {
  title: string;
  message: ReactNode;
  confirmLabel: string;
  /** The confirm button's look: danger (the default) for removals, primary for an action that is not destructive. */
  confirmVariant?: "danger" | "primary";
  busy?: boolean;
  error?: string | null;
  onConfirm: () => void;
  onCancel: () => void;
};

export function ConfirmDialog({ title, message, confirmLabel, confirmVariant = "danger", busy, error, onConfirm, onCancel }: ConfirmProps) {
  return (
    <Modal title={title} onClose={busy ? () => undefined : onCancel}>
      <div className="text-sm text-zinc-600 dark:text-zinc-400">{message}</div>
      {error ? <div className="mt-3"><InlineError>{error}</InlineError></div> : null}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button variant={confirmVariant} onClick={onConfirm} disabled={busy} autoFocus>
          {busy ? "Working…" : confirmLabel}
        </Button>
      </div>
    </Modal>
  );
}
