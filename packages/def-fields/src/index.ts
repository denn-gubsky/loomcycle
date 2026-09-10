// @loomcycle/def-fields — the shareable editor surface for loomcycle substrate
// primitives.
//
// The package is deliberately PRESENTATIONAL: it has no client, no fetch, no
// knowledge of how a def is persisted. It takes a registry (what the parameters
// are), a sparse overlay value, and an onChange — so the loomcycle Web UI can
// wire it to `AgentDef create/fork` while loomboard wires the same components to
// a canvas node's own store, and neither inherits the other's data layer.
//
// Styles are NOT imported here: a consumer opts in with
// `import "@loomcycle/def-fields/styles.css"`, which keeps a host that supplies
// its own palette from having to fight ours.

export { FoldedFieldList } from "./FoldedFieldList";
export type { FoldedFieldListProps } from "./FoldedFieldList";

export { FieldRow } from "./components/FieldRow";
export type { FieldRowProps } from "./components/FieldRow";

export { FieldControl } from "./components/FieldControl";
export type { FieldControlProps } from "./components/FieldControl";

export type {
  DefRegistry,
  DefValue,
  FieldGroupSpec,
  FieldSpec,
  FieldType,
} from "./types";

// Overlay helpers. A host that builds its own arrangement of FieldRows still
// needs the sparse-overlay semantics (absent = inherit, present-zero = a real
// setting), and re-deriving them per host is exactly how that distinction gets
// lost.
export {
  clearField,
  countSet,
  fieldsInGroup,
  isSet,
  matchesQuery,
  setField,
  visibleFields,
} from "./lib/value";

// Registries, one per substrate kind.
export { agentDefRegistry, AGENTDEF_EXCLUDED } from "./registries/agentdef";
