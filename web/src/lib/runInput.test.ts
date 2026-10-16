import { describe, expect, it } from "vitest";
import { buildInput, draftFromInput, emptyDraft, inputFields, parseWholeInput } from "./runInput";

const schema = {
  type: "object",
  properties: {
    task: { type: "string", description: "What to build" },
    retries: { type: "integer" },
    ratio: { type: "number" },
    dry_run: { type: "boolean" },
    files: { type: "array" },
    options: { type: "object" },
    loose: {},
  },
  required: ["task", "retries"],
};

describe("inputFields", () => {
  it("makes one field per property with its type, requiredness and description", () => {
    const fields = inputFields(schema);
    expect(fields.map((f) => [f.name, f.type, f.required])).toEqual([
      ["task", "string", true],
      ["retries", "integer", true],
      ["ratio", "number", false],
      ["dry_run", "boolean", false],
      ["files", "json", false],
      ["options", "json", false],
      ["loose", "json", false],
    ]);
    expect(fields[0].description).toBe("What to build");
  });

  it("returns nothing for a schema without properties", () => {
    expect(inputFields(undefined)).toEqual([]);
    expect(inputFields({ type: "object" })).toEqual([]);
    expect(inputFields("nope")).toEqual([]);
  });
});

describe("buildInput", () => {
  const fields = inputFields(schema);

  it("turns the draft into typed values and leaves empty optional fields out", () => {
    const draft = { ...emptyDraft(fields), task: "build it", retries: "3", ratio: "0.5", dry_run: true, files: '["a.py"]' };
    const { input, errors } = buildInput(fields, draft);
    expect(errors).toEqual({});
    expect(input).toEqual({ task: "build it", retries: 3, ratio: 0.5, dry_run: true, files: ["a.py"] });
  });

  it("always sends the boolean, even when unchecked", () => {
    const { input } = buildInput(fields, { ...emptyDraft(fields), task: "x", retries: "1" });
    expect(input.dry_run).toBe(false);
  });

  it("reports a message per field that cannot be used", () => {
    const draft = { ...emptyDraft(fields), retries: "1.5", ratio: "lots", files: "[1," };
    const { errors } = buildInput(fields, draft);
    expect(errors.task).toBe("This field is required");
    expect(errors.retries).toBe("Enter a whole number");
    expect(errors.ratio).toBe("Enter a number");
    expect(errors.files).toMatch(/^Not valid JSON/);
  });

  it("requires required fields that are blank", () => {
    const { errors } = buildInput(fields, { ...emptyDraft(fields), task: "   ", retries: "" });
    expect(Object.keys(errors).sort()).toEqual(["retries", "task"]);
  });
});

describe("parseWholeInput", () => {
  it("accepts an object, an empty box and refuses everything else", () => {
    expect(parseWholeInput('{"a": 1}')).toEqual({ input: { a: 1 } });
    expect(parseWholeInput("  ")).toEqual({ input: {} });
    expect(parseWholeInput("[1]").error).toBe("The input must be a JSON object");
    expect(parseWholeInput("{").error).toMatch(/^Not valid JSON/);
  });
});

describe("draftFromInput", () => {
  it("reproduces a saved input, so building it again gives the same values", () => {
    const fields = inputFields(schema);
    const input = { task: "ship it", retries: 3, ratio: 0.5, dry_run: true, files: ["a.go"], options: { fast: true } };
    const draft = draftFromInput(fields, input);
    expect(draft.task).toBe("ship it");
    expect(draft.retries).toBe("3");
    expect(draft.dry_run).toBe(true);
    expect(buildInput(fields, draft)).toEqual({ input: { ...input }, errors: {} });
  });

  it("leaves fields the input does not have empty, and copes with no input at all", () => {
    const fields = inputFields(schema);
    expect(draftFromInput(fields, { task: "x" })).toEqual({ ...emptyDraft(fields), task: "x" });
    expect(draftFromInput(fields, null)).toEqual(emptyDraft(fields));
  });
});
