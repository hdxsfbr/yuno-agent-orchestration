import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, createSkill, updateSkill, type Skill, type SkillInput } from "@/lib/api";
import { SKILL_NAME, errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import Button from "../Button";
import { Modal } from "../Modal";
import { InlineError, errorText } from "../QueryState";
import { TextAreaField, TextField } from "../fields";

const NAME_HINT = "Lowercase words separated by hyphens, such as code-review.";

/** Create or edit a skill. A skill keeps its name once saved, because agents refer to it by name. */
export default function SkillDialog({ skill, onClose }: { skill?: Skill; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(skill?.name ?? "");
  const [description, setDescription] = useState(skill?.description ?? "");
  const [body, setBody] = useState(skill?.body ?? "");
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (input: SkillInput) => (skill ? updateSkill(skill.id, input) : createSkill(input)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["skills"] });
      // Agents that use the skill are updated with it.
      void qc.invalidateQueries({ queryKey: ["agents"] });
      onClose();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (save.isPending) return;
    setServerErrors({});
    setFormError(null);
    const found: FieldErrors = {};
    const n = name.trim();
    if (n === "") found.name = ["Name is required"];
    else if (!SKILL_NAME.test(n)) found.name = [NAME_HINT];
    if (body.trim() === "") found.body = ["Write the instructions the skill gives an agent"];
    setClientErrors(found);
    if (Object.keys(found).length === 0) save.mutate({ name: n, description: description.trim(), body });
  };

  const errors = (path: string) => [...errorsFor(clientErrors, path), ...errorsFor(serverErrors, path)];
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !["name", "description", "body"].includes(k.split(".")[0]))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  const showSummary = formError && unplaced.length === 0 && Object.keys(serverErrors).length === 0;

  return (
    <Modal title={skill ? `Edit ${skill.name}` : "New skill"} onClose={save.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} noValidate className="space-y-4">
        <TextField
          label="Name"
          required
          mono
          value={name}
          onChange={setName}
          disabled={!!skill}
          placeholder="code-review"
          hint={skill ? "The name cannot change because agents refer to the skill by name." : NAME_HINT}
          errors={errors("name")}
        />
        <TextField label="Description" hint="What the skill is for, in a sentence." value={description} onChange={setDescription} errors={errors("description")} />
        <TextAreaField
          label="Instructions"
          required
          rows={14}
          value={body}
          onChange={setBody}
          placeholder={"# When to use this skill\n\nWrite the steps the agent should follow, in Markdown."}
          hint="Markdown. The agent reads this when it uses the skill."
          errors={errors("body")}
        />
        {unplaced.map((m, i) => (
          <InlineError key={i}>{m}</InlineError>
        ))}
        {showSummary ? <InlineError>{formError}</InlineError> : null}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={save.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={save.isPending}>
            {save.isPending ? "Saving…" : skill ? "Save changes" : "Create skill"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
