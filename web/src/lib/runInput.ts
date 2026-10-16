// The form behind "Run": one field per property of the workflow's input schema. Strings,
// numbers and booleans get their own control; every other property is raw JSON.

export type InputFieldType = "string" | "number" | "integer" | "boolean" | "json";

export type InputField = {
  name: string;
  type: InputFieldType;
  required: boolean;
  description?: string;
};

export type InputDraft = Record<string, string | boolean>;

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

export function inputFields(schema: unknown): InputField[] {
  if (!isRecord(schema) || !isRecord(schema.properties)) return [];
  const required = Array.isArray(schema.required) ? schema.required.filter((r): r is string => typeof r === "string") : [];
  return Object.entries(schema.properties).map(([name, prop]) => {
    const p = isRecord(prop) ? prop : {};
    const type: InputFieldType = p.type === "string" || p.type === "number" || p.type === "integer" || p.type === "boolean" ? p.type : "json";
    return {
      name,
      type,
      required: required.includes(name),
      ...(typeof p.description === "string" ? { description: p.description } : {}),
    };
  });
}

export function emptyDraft(fields: InputField[]): InputDraft {
  return Object.fromEntries(fields.map((f) => [f.name, f.type === "boolean" ? false : ""]));
}

export type BuiltInput = { input: Record<string, unknown>; errors: Record<string, string> };

/** The input object for the draft, or a message per field that cannot be turned into a value. */
export function buildInput(fields: InputField[], draft: InputDraft): BuiltInput {
  const input: Record<string, unknown> = {};
  const errors: Record<string, string> = {};
  for (const f of fields) {
    const raw = draft[f.name];
    if (f.type === "boolean") {
      input[f.name] = raw === true;
      continue;
    }
    const text = typeof raw === "string" ? raw : "";
    if (text.trim() === "") {
      if (f.required) errors[f.name] = "This field is required";
      continue;
    }
    if (f.type === "string") {
      input[f.name] = text;
    } else if (f.type === "number" || f.type === "integer") {
      const n = Number(text);
      if (!Number.isFinite(n)) errors[f.name] = "Enter a number";
      else if (f.type === "integer" && !Number.isInteger(n)) errors[f.name] = "Enter a whole number";
      else input[f.name] = n;
    } else {
      try {
        input[f.name] = JSON.parse(text);
      } catch (e) {
        errors[f.name] = `Not valid JSON: ${e instanceof Error ? e.message : String(e)}`;
      }
    }
  }
  return { input, errors };
}

/** Input for a workflow whose schema names no properties: the whole object as JSON. */
export function parseWholeInput(text: string): { input?: Record<string, unknown>; error?: string } {
  if (text.trim() === "") return { input: {} };
  try {
    const v: unknown = JSON.parse(text);
    return isRecord(v) ? { input: v } : { error: "The input must be a JSON object" };
  } catch (e) {
    return { error: `Not valid JSON: ${e instanceof Error ? e.message : String(e)}` };
  }
}

/** The draft that reproduces an input already saved, so a form can open on it. */
export function draftFromInput(fields: InputField[], input: Record<string, unknown> | null | undefined): InputDraft {
  const draft = emptyDraft(fields);
  for (const f of fields) {
    const v = input?.[f.name];
    if (v === undefined || v === null) continue;
    if (f.type === "boolean") draft[f.name] = v === true;
    else if (f.type === "json") draft[f.name] = JSON.stringify(v, null, 2);
    else draft[f.name] = String(v);
  }
  return draft;
}
