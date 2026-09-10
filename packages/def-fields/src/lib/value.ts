import type { DefRegistry, DefValue, FieldSpec } from "../types";

// isSet reports whether the overlay carries an explicit value for this key.
//
// The unset-vs-zero distinction is load-bearing: the substrate stores several
// knobs as POINTERS precisely so `0` / `false` / `""` are real, inheritable-over
// settings distinct from "not configured". So presence is decided by key
// presence, never by truthiness — `value[key] ?? fallback` would silently
// promote an explicit 0 to "inherit".
export function isSet(value: DefValue, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key) && value[key] !== undefined;
}

/** setField returns a new overlay with `key` explicitly set. */
export function setField(value: DefValue, key: string, next: unknown): DefValue {
  return { ...value, [key]: next };
}

/** clearField returns a new overlay with `key` removed — i.e. back to inherit.
 *  Deleting (not writing null) is what the substrate reads as "unset", so the
 *  parent/operator value survives the merge. */
export function clearField(value: DefValue, key: string): DefValue {
  if (!isSet(value, key)) return value;
  const next = { ...value };
  delete next[key];
  return next;
}

/** countSet is the "(n)" badge on a folded group header. */
export function countSet(value: DefValue, fields: readonly FieldSpec[]): number {
  let n = 0;
  for (const f of fields) if (isSet(value, f.key)) n++;
  return n;
}

/** fieldsInGroup preserves registry order, which is the display order. */
export function fieldsInGroup(reg: DefRegistry, group: string): FieldSpec[] {
  return reg.fields.filter((f) => f.group === group);
}

// matchesQuery powers the list's search box. It matches the KEY, the label and
// the hint, so an operator who remembers "the thing about burst budget" finds
// the field without knowing its key.
export function matchesQuery(f: FieldSpec, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return (
    f.key.toLowerCase().includes(q) ||
    f.label.toLowerCase().includes(q) ||
    f.hint.toLowerCase().includes(q)
  );
}

/** visibleFields applies the search + modified-only filters together. */
export function visibleFields(
  fields: readonly FieldSpec[],
  value: DefValue,
  query: string,
  modifiedOnly: boolean,
): FieldSpec[] {
  return fields.filter(
    (f) => matchesQuery(f, query) && (!modifiedOnly || isSet(value, f.key)),
  );
}
