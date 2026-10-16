import { useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { setToken, useSignInRequired, useToken } from "@/lib/auth";
import Button from "./Button";
import { Field, control } from "./fields";
import { Modal } from "./Modal";

function SignInForm({ rejected }: { rejected: boolean }) {
  const qc = useQueryClient();
  const [value, setValue] = useState("");
  const errors = rejected ? ["The platform did not accept that token. Check it and try again."] : [];

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (!value.trim()) return;
    setToken(value);
    // Whatever failed for want of the token is asked again, not left showing an error.
    void qc.refetchQueries({ type: "active", predicate: (q) => q.state.status === "error" });
  };

  return (
    // The platform cannot be used without a token, so there is nothing to dismiss to.
    <Modal title="Sign in" onClose={() => undefined}>
      <form onSubmit={save} className="space-y-4">
        <p className="text-sm text-zinc-600 dark:text-zinc-400">This platform needs its API token. Enter it to continue.</p>
        <Field label="Platform token" errors={errors}>
          {(p) => <input {...p} type="password" autoComplete="off" autoFocus value={value} onChange={(e) => setValue(e.target.value)} className={control(errors.length > 0)} />}
        </Field>
        <div className="flex justify-end">
          <Button type="submit" variant="primary" disabled={!value.trim()}>
            Save
          </Button>
        </div>
      </form>
    </Modal>
  );
}

/** Asks for the platform token when the API refuses a request for want of one. */
export default function SignInDialog() {
  const required = useSignInRequired();
  const token = useToken();
  if (!required) return null;
  return <SignInForm rejected={token !== ""} />;
}
