import { describe, expect, it } from "vitest";
import type { ChunkRow } from "../api";
import {
  buildInput,
  chunkOptions,
  chunkPickerEnabled,
  entryForm,
  entrySchema,
  fieldsFromSchema,
  initialValues,
  setFieldValue,
} from "./teamStart";

// The pcparts-style form from the agent-teams help article: a document picker
// and a chunk picker that follows it.
const partsSchema = {
  type: "object",
  required: ["document_id", "chunk_id"],
  properties: {
    document_id: {
      type: "string",
      title: "Document",
      "x-loomcycle-picker": { kind: "document", scope: "tenant", under_path: "/parts" },
    },
    chunk_id: {
      type: "string",
      title: "Part",
      "x-loomcycle-picker": { kind: "chunk", document: "document_id", depth: 1 },
    },
  },
};

function team(entryHandler: Record<string, unknown>) {
  return {
    entry: "start",
    states: [
      { state: "start", handler: entryHandler },
      { state: "done", handler: { kind: "terminal" } },
    ],
    transitions: [{ from: "start", to: "done", on: "success" }],
  };
}

describe("entrySchema", () => {
  it("is read from an input-state entry", () => {
    expect(entrySchema(team({ kind: "input", schema: partsSchema }))).toEqual(partsSchema);
  });

  it("is read from a starter entry whose source is the input", () => {
    const def = team({ kind: "starter", source: { kind: "input" }, schema: partsSchema });
    expect(entrySchema(def)).toEqual(partsSchema);
  });

  it("is absent for an entry that does not read the walk's input", () => {
    // An agent entry, or a starter fed by a channel, has no input form even
    // when a schema is present — the server does not check one either.
    expect(entrySchema(team({ kind: "agent", agent: "a", schema: partsSchema }))).toBeUndefined();
    expect(entrySchema(team({ kind: "starter", source: { kind: "channel" }, schema: partsSchema }))).toBeUndefined();
  });

  it("comes from the entry state only, never a later one", () => {
    const def = {
      entry: "work",
      states: [
        { state: "work", handler: { kind: "agent", agent: "a" } },
        { state: "later", handler: { kind: "input", schema: partsSchema } },
      ],
    };
    expect(entrySchema(def)).toBeUndefined();
  });
});

describe("entryForm", () => {
  it("is a plain text box when the entry has no schema", () => {
    expect(entryForm(team({ kind: "input" }))).toEqual({ kind: "text" });
    expect(entryForm(team({ kind: "agent", agent: "a" }))).toEqual({ kind: "text" });
    expect(entryForm(undefined)).toEqual({ kind: "text" });
  });

  it("is a plain text box when the schema declares no fields", () => {
    expect(entryForm(team({ kind: "input", schema: { type: "object" } }))).toEqual({ kind: "text" });
  });

  it("is a field form, in declaration order, for a schema entry", () => {
    const f = entryForm(team({ kind: "input", schema: partsSchema }));
    expect(f.kind).toBe("schema");
    expect(f.kind === "schema" && f.fields.map((x) => x.name)).toEqual(["document_id", "chunk_id"]);
  });
});

describe("fieldsFromSchema", () => {
  it("maps each property type to its control, with title, description and required", () => {
    const fields = fieldsFromSchema({
      required: ["title"],
      properties: {
        title: { type: "string", title: "Title", description: "What to call it" },
        count: { type: "integer" },
        ratio: { type: "number" },
        draft: { type: "boolean" },
        tone: { type: "string", enum: ["formal", "casual"] },
        meta: { type: "object" },
        maybe: { type: ["string", "null"] },
        untyped: {},
      },
    });
    expect(fields.map((f) => [f.name, f.kind, f.required])).toEqual([
      ["title", "text", true],
      ["count", "integer", false],
      ["ratio", "number", false],
      ["draft", "boolean", false],
      ["tone", "enum", false],
      ["meta", "json", false],
      ["maybe", "text", false],
      ["untyped", "json", false],
    ]);
    expect(fields[0]).toMatchObject({ label: "Title", description: "What to call it" });
    expect(fields[1].label).toBe("count");
    expect(fields[4].enumValues).toEqual(["formal", "casual"]);
  });

  it("gives a required name with no property a text box", () => {
    const fields = fieldsFromSchema({ type: "object", required: ["document_id", "chunk_id"] });
    expect(fields.map((f) => [f.name, f.kind, f.required])).toEqual([
      ["document_id", "text", true],
      ["chunk_id", "text", true],
    ]);
  });

  it("reads document and chunk pickers, the chunk picker taking its document's scope", () => {
    const [doc, chunk] = fieldsFromSchema(partsSchema);
    expect(doc).toMatchObject({ kind: "document", picker: { kind: "document", scope: "tenant", underPath: "/parts" } });
    // The chunk picker names no scope, so it reads the store of the document it
    // follows.
    expect(chunk).toMatchObject({ kind: "chunk", picker: { kind: "chunk", document: "document_id", scope: "tenant", depth: 1 } });
  });

  it("defaults a picker's scope to user", () => {
    const [doc, chunk] = fieldsFromSchema({
      properties: {
        d: { type: "string", "x-loomcycle-picker": { kind: "document" } },
        c: { type: "string", "x-loomcycle-picker": { kind: "chunk", document: "d" } },
      },
    });
    expect(doc.picker).toEqual({ kind: "document", scope: "user", underPath: undefined });
    expect(chunk.picker).toEqual({ kind: "chunk", document: "d", scope: "user", depth: undefined });
  });

  it("falls back to a text box for reserved or unknown picker kinds", () => {
    const fields = fieldsFromSchema({
      properties: {
        mem: { type: "string", "x-loomcycle-picker": { kind: "memory" } },
        ch: { type: "string", "x-loomcycle-picker": { kind: "channel" } },
        ag: { type: "string", "x-loomcycle-picker": { kind: "agent" } },
        odd: { type: "string", "x-loomcycle-picker": { kind: "spreadsheet" } },
      },
    });
    expect(fields.map((f) => [f.kind, f.picker])).toEqual([
      ["text", undefined],
      ["text", undefined],
      ["text", undefined],
      ["text", undefined],
    ]);
  });

  it("falls back to a text box for a chunk picker with no document field to follow", () => {
    // It follows a field that is missing, or that is not a document picker.
    const fields = fieldsFromSchema({
      properties: {
        plain: { type: "string" },
        a: { type: "string", "x-loomcycle-picker": { kind: "chunk", document: "missing" } },
        b: { type: "string", "x-loomcycle-picker": { kind: "chunk", document: "plain" } },
      },
    });
    expect(fields.map((f) => [f.name, f.kind, f.picker])).toEqual([
      ["plain", "text", undefined],
      ["a", "text", undefined],
      ["b", "text", undefined],
    ]);
  });
});

describe("the chunk picker", () => {
  const fields = fieldsFromSchema(partsSchema);

  it("is disabled until its document field has a value", () => {
    const chunk = fields[1];
    expect(chunkPickerEnabled(chunk, initialValues(fields))).toBe(false);
    expect(chunkPickerEnabled(chunk, { document_id: "  ", chunk_id: "" })).toBe(false);
    expect(chunkPickerEnabled(chunk, { document_id: "doc1", chunk_id: "" })).toBe(true);
    // Every other field is always enabled.
    expect(chunkPickerEnabled(fields[0], initialValues(fields))).toBe(true);
  });

  it("clears the chosen chunk when the document changes", () => {
    let v = initialValues(fields);
    v = setFieldValue(fields, v, "document_id", "doc1");
    v = setFieldValue(fields, v, "chunk_id", "c1");
    expect(v).toEqual({ document_id: "doc1", chunk_id: "c1" });
    v = setFieldValue(fields, v, "document_id", "doc2");
    expect(v).toEqual({ document_id: "doc2", chunk_id: "" });
  });

  it("keeps the chosen chunk when the same document is set again", () => {
    const v = setFieldValue(fields, { document_id: "doc1", chunk_id: "c1" }, "document_id", "doc1");
    expect(v.chunk_id).toBe("c1");
  });
});

describe("buildInput", () => {
  it("sends the filled-in fields as a typed JSON object", () => {
    const fields = fieldsFromSchema({
      properties: {
        s: { type: "string" },
        n: { type: "number" },
        i: { type: "integer" },
        b: { type: "boolean" },
        e: { enum: ["x", 2, { k: true }] },
        j: { type: "object" },
      },
    });
    const r = buildInput(fields, { s: "hello", n: "1.5", i: "3", b: true, e: "2", j: '{"a":[1]}' });
    expect(r.ok && JSON.parse(r.input)).toEqual({ s: "hello", n: 1.5, i: 3, b: true, e: { k: true }, j: { a: [1] } });
  });

  it("omits an empty field, so the server names a missing required one", () => {
    const fields = fieldsFromSchema(partsSchema);
    const r = buildInput(fields, { document_id: "doc1", chunk_id: "" });
    expect(r.ok && JSON.parse(r.input)).toEqual({ document_id: "doc1" });
  });

  it("sends an unchecked box as false", () => {
    const fields = fieldsFromSchema({ properties: { b: { type: "boolean" } } });
    const r = buildInput(fields, initialValues(fields));
    expect(r.ok && JSON.parse(r.input)).toEqual({ b: false });
  });

  it("reports a JSON field that does not parse", () => {
    const fields = fieldsFromSchema({ properties: { meta: { type: "object", title: "Meta" } } });
    expect(buildInput(fields, { meta: "{nope" })).toEqual({ ok: false, error: "Meta: not valid JSON" });
  });
});

describe("chunkOptions", () => {
  const row = (id: string, parent: string | undefined, position: number, title = id): ChunkRow => ({
    id,
    document_id: "d",
    parent_id: parent,
    position,
    title,
    revision: 1,
  });
  // root → s1 (→ s1a), s2; delivered out of order.
  const chunks = [row("s2", "root", 1), row("s1a", "s1", 0), row("root", undefined, 0), row("s1", "root", 0)];

  it("lists the chunk tree in order, without the root", () => {
    expect(chunkOptions(chunks)).toEqual([
      { id: "s1", title: "s1", depth: 1 },
      { id: "s1a", title: "s1a", depth: 2 },
      { id: "s2", title: "s2", depth: 1 },
    ]);
  });

  it("keeps only top-level sections at depth 1", () => {
    expect(chunkOptions(chunks, 1).map((o) => o.id)).toEqual(["s1", "s2"]);
  });

  it("terminates on a cycle in parent pointers", () => {
    const cyclic = [row("root", undefined, 0), row("a", "b", 0), row("b", "a", 0)];
    expect(chunkOptions(cyclic)).toEqual([]);
  });
});
