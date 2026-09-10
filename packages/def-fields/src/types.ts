// The field registry: ONE declarative description of a substrate primitive's
// editable parameters, from which every surface is derived — the grouped form,
// the folded (accordion) list, the per-parameter hints, and the overlay
// read/write.
//
// WHY a registry rather than hand-written controls: the pre-registry editor
// wrote each field three times (state hook, JSX, overlay builder) plus a
// hand-maintained "which keys are covered" set. That set drifted — two fields
// shipped rendered-and-emitted but unregistered, so the advanced-overlay logic
// misclassified them. With a registry, a field is one entry and "rendered but
// not saved" is not expressible.

// FieldType selects the editor control. Keep this list small: a new type is a
// new control to maintain in every surface, so prefer composing `object` /
// `object-array` over inventing bespoke types.
export type FieldType =
  | "text"
  | "textarea"
  | "int"
  // A real number ("temperature: 0.7"). Separate from int only so the control
  // accepts a fractional step — the browser's default number step is 1, which
  // marks 0.7 invalid.
  | "float"
  | "bool"
  | "enum"
  | "string-list"
  | "kv"
  | "object"
  | "object-array"
  // A single field whose VALUE is genuinely free-form structured data (a JSON
  // Schema, a tier→candidates map). Deliberately per-field and typed — not the
  // removed whole-overlay JSON box, which hid every uncovered key behind one
  // unvalidated textarea.
  | "json";

export interface FieldSpec {
  /** The overlay key this field reads and writes. Must match the substrate's
   *  persisted shape exactly — the drift test asserts the whole set. */
  key: string;
  label: string;
  /** Accordion section this field folds under. Must be one of registry.groups. */
  group: string;
  type: FieldType;
  /** One-sentence operator-facing explanation. Rendered in BOTH the form and the
   *  folded list, so it is written once here and never duplicated per surface. */
  hint: string;
  placeholder?: string;
  /** enum only: the allowed values. */
  options?: readonly string[];
  /** int / float only: inclusive bounds, surfaced to the control and to validation. */
  min?: number;
  max?: number;
  /** What an UNSET field means, e.g. "inherits the parent def / operator yaml".
   *  Shown on the inherit affordance so an empty box is never ambiguous. */
  unsetMeans?: string;
  /** object / object-array: the child fields. */
  fields?: readonly FieldSpec[];
  /** Folded away by default even inside its group — rarely-tuned knobs. */
  advanced?: boolean;
}

export interface FieldGroupSpec {
  name: string;
  /** Section-level context, shown once under the group header. */
  hint?: string;
}

export interface DefRegistry {
  /** Substrate kind: "agentdef" | "mcpserverdef" | … */
  kind: string;
  /** Section order for the folded list. Every field.group must appear here. */
  groups: readonly FieldGroupSpec[];
  fields: readonly FieldSpec[];
}

/** A sparse overlay: a key that is ABSENT means "inherit", which is different
 *  from a key present with a zero value (0 / "" / false are real settings the
 *  substrate's pointer fields preserve). Every editor honours that distinction. */
export type DefValue = Record<string, unknown>;
