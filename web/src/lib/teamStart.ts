// teamStart — the pure half of starting a team from the Teams page: read the
// entry state's input form, map its JSON Schema onto form fields, and build the
// walk's input from what the person filled in.
//
// The server checks the input (top-level type, `required`, one level of
// property types) and names the bad field in a 422, so this module does not
// re-validate: it only marks required fields and builds the value. A field the
// person left empty is OMITTED rather than sent as "" — that way a missing
// required field is refused by the server with its name, not accepted as an
// empty string.

import type { ChunkRow, DocScope } from "../api";

// PickerSpec is a field's `x-loomcycle-picker`: how a person chooses its value.
// The value itself is always the plain id string.
export type PickerSpec =
  | { kind: "document"; scope: DocScope; underPath?: string }
  // `document` names the sibling field holding the document id; the chunk list
  // follows it. `scope` falls back to that sibling's picker scope.
  | { kind: "chunk"; document: string; scope: DocScope; depth?: number };

export type FieldKind = "text" | "number" | "integer" | "boolean" | "enum" | "json" | "document" | "chunk";

export interface FormField {
  name: string;
  label: string;
  description?: string;
  required: boolean;
  kind: FieldKind;
  // kind "enum": the allowed values, in schema order. The form stores the
  // selected INDEX so a non-string enum value round-trips unchanged.
  enumValues?: unknown[];
  picker?: PickerSpec;
}

// FieldValue: a checkbox holds a boolean, everything else the raw text the
// person typed or picked (an enum holds its option index, "" = unset).
export type FieldValue = string | boolean;
export type FormValues = Record<string, FieldValue>;

// EntryForm is what the Run dialog renders: a schema form, or — when the entry
// declares no usable form — one multiline box whose text is sent as-is.
export type EntryForm = { kind: "schema"; fields: FormField[] } | { kind: "text" };

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);

// entrySchema returns the entry state's `schema` when the entry is where the
// walk's input arrives: an `input` state, or a starter whose source is the
// input. Any other entry has no form (the server does not check one either).
export function entrySchema(definition: unknown): Obj | undefined {
  if (!isObj(definition) || typeof definition.entry !== "string") return undefined;
  const states = Array.isArray(definition.states) ? definition.states : [];
  const entry = states.find((s) => isObj(s) && s.state === definition.entry);
  if (!isObj(entry) || !isObj(entry.handler)) return undefined;
  const h = entry.handler;
  const readsInput = h.kind === "input" || (h.kind === "starter" && isObj(h.source) && h.source.kind === "input");
  return readsInput && isObj(h.schema) ? h.schema : undefined;
}

// entryForm maps a stored team definition onto the form the Run dialog shows.
export function entryForm(definition: unknown): EntryForm {
  const schema = entrySchema(definition);
  const fields = schema ? fieldsFromSchema(schema) : [];
  return fields.length > 0 ? { kind: "schema", fields } : { kind: "text" };
}

// fieldsFromSchema reads the top-level `properties` (in declaration order),
// then any `required` name that has no property — it still needs a box, typed
// as text since nothing says otherwise. Nested schemas are not expanded: a
// field that is not a scalar becomes a JSON text area.
export function fieldsFromSchema(schema: Obj): FormField[] {
  const props = isObj(schema.properties) ? schema.properties : {};
  const required = new Set(
    Array.isArray(schema.required) ? schema.required.filter((r): r is string => typeof r === "string") : [],
  );
  // A chunk picker with no scope of its own reads the store of the document
  // field it follows: the chunks live where that document does.
  const docScopes = new Map<string, DocScope>();
  for (const [name, p] of Object.entries(props)) {
    const picker = isObj(p) ? pickerOf(p["x-loomcycle-picker"], "user") : undefined;
    if (picker?.kind === "document") docScopes.set(name, picker.scope);
  }
  const fields: FormField[] = Object.entries(props).map(([name, p]) =>
    fieldFor(name, isObj(p) ? p : {}, required.has(name), docScopes),
  );
  for (const name of required) {
    if (!(name in props)) fields.push({ name, label: name, required: true, kind: "text" });
  }
  return fields;
}

function fieldFor(name: string, p: Obj, required: boolean, docScopes: Map<string, DocScope>): FormField {
  const base = {
    name,
    label: typeof p.title === "string" && p.title.trim() ? p.title : name,
    description: typeof p.description === "string" && p.description.trim() ? p.description : undefined,
    required,
  };
  const raw = p["x-loomcycle-picker"];
  const followed = isObj(raw) && typeof raw.document === "string" ? docScopes.get(raw.document) : undefined;
  const picker = pickerOf(raw, followed ?? "user");
  // A chunk picker follows a sibling document field; with no such field there
  // is nothing to follow, so it is an ordinary field of its declared type.
  if (picker && (picker.kind === "document" || docScopes.has(picker.document))) {
    return { ...base, kind: picker.kind, picker };
  }
  if (Array.isArray(p.enum) && p.enum.length > 0) return { ...base, kind: "enum", enumValues: p.enum };
  switch (primaryType(p.type)) {
    case "string":
      return { ...base, kind: "text" };
    case "number":
      return { ...base, kind: "number" };
    case "integer":
      return { ...base, kind: "integer" };
    case "boolean":
      return { ...base, kind: "boolean" };
    default:
      return { ...base, kind: "json" };
  }
}

// primaryType picks the type a control is built for: the name, or the first
// non-null name of a list (["string","null"] is a string box).
function primaryType(t: unknown): string | undefined {
  if (typeof t === "string") return t;
  if (Array.isArray(t)) return t.find((x): x is string => typeof x === "string" && x !== "null");
  return undefined;
}

// pickerOf reads `x-loomcycle-picker`. Only document and chunk are rendered;
// memory/channel/agent are reserved and anything unknown is a plain field.
// defaultScope applies when the picker names no scope.
function pickerOf(raw: unknown, defaultScope: DocScope): PickerSpec | undefined {
  if (!isObj(raw)) return undefined;
  const scope: DocScope = raw.scope === "tenant" ? "tenant" : raw.scope === "user" ? "user" : defaultScope;
  if (raw.kind === "document") {
    const underPath = typeof raw.under_path === "string" && raw.under_path.trim() ? raw.under_path : undefined;
    return { kind: "document", scope, underPath };
  }
  if (raw.kind === "chunk" && typeof raw.document === "string" && raw.document) {
    const depth = typeof raw.depth === "number" && Number.isInteger(raw.depth) && raw.depth > 0 ? raw.depth : undefined;
    return { kind: "chunk", document: raw.document, scope, depth };
  }
  return undefined;
}

// initialValues: unchecked boxes, empty everything else.
export function initialValues(fields: FormField[]): FormValues {
  const v: FormValues = {};
  for (const f of fields) v[f.name] = f.kind === "boolean" ? false : "";
  return v;
}

// setFieldValue sets one field and clears every chunk picker that follows it: a
// chunk chosen from the previous document is not a chunk of the new one.
export function setFieldValue(fields: FormField[], values: FormValues, name: string, value: FieldValue): FormValues {
  const next: FormValues = { ...values, [name]: value };
  if (values[name] !== value) {
    for (const f of fields) {
      if (f.picker?.kind === "chunk" && f.picker.document === name) next[f.name] = "";
    }
  }
  return next;
}

// chunkPickerEnabled: a chunk picker waits for its document to be chosen.
export function chunkPickerEnabled(field: FormField, values: FormValues): boolean {
  if (field.picker?.kind !== "chunk") return true;
  const doc = values[field.picker.document];
  return typeof doc === "string" && doc.trim() !== "";
}

// buildInput turns the filled-in form into the walk's input string (a JSON
// object). Empty fields are omitted; a number or JSON box that does not parse
// is reported here because there is no value to send for it.
export function buildInput(fields: FormField[], values: FormValues): { ok: true; input: string } | { ok: false; error: string } {
  const out: Obj = {};
  for (const f of fields) {
    const v = values[f.name];
    if (f.kind === "boolean") {
      out[f.name] = v === true;
      continue;
    }
    const s = typeof v === "string" ? v : "";
    if (s.trim() === "") continue;
    switch (f.kind) {
      case "number":
      case "integer": {
        const n = Number(s);
        if (!Number.isFinite(n)) return { ok: false, error: `${f.label}: "${s}" is not a number` };
        out[f.name] = n;
        break;
      }
      case "enum": {
        const i = Number(s);
        if (f.enumValues && Number.isInteger(i) && i >= 0 && i < f.enumValues.length) out[f.name] = f.enumValues[i];
        break;
      }
      case "json":
        try {
          out[f.name] = JSON.parse(s);
        } catch {
          return { ok: false, error: `${f.label}: not valid JSON` };
        }
        break;
      default:
        out[f.name] = s;
    }
  }
  return { ok: true, input: JSON.stringify(out) };
}

export interface ChunkOption {
  id: string;
  title: string;
  depth: number;
}

// chunkOptions lists a document's chunks for a chunk picker, in tree order,
// without the root (the root is the document itself, not a part of it). With
// maxDepth only chunks that many levels under the root are kept — 1 is the
// top-level sections.
export function chunkOptions(chunks: readonly ChunkRow[], maxDepth?: number): ChunkOption[] {
  const ids = new Set(chunks.map((c) => c.id));
  const children = new Map<string, ChunkRow[]>();
  const roots: ChunkRow[] = [];
  for (const c of chunks) {
    if (c.parent_id && ids.has(c.parent_id)) {
      const list = children.get(c.parent_id) ?? [];
      list.push(c);
      children.set(c.parent_id, list);
    } else {
      roots.push(c);
    }
  }
  const byPos = (a: ChunkRow, b: ChunkRow) => a.position - b.position;
  const out: ChunkOption[] = [];
  // A visited set: parent pointers are stored data, and a cycle
  // must not hang the page.
  const seen = new Set<string>();
  const walk = (c: ChunkRow, depth: number) => {
    if (seen.has(c.id)) return;
    seen.add(c.id);
    if (depth > 0 && (maxDepth === undefined || depth <= maxDepth)) {
      out.push({ id: c.id, title: c.title || c.id, depth });
    }
    if (maxDepth !== undefined && depth >= maxDepth) return;
    for (const k of (children.get(c.id) ?? []).sort(byPos)) walk(k, depth + 1);
  };
  for (const r of roots.sort(byPos)) walk(r, 0);
  return out;
}
