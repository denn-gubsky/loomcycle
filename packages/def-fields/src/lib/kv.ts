// The row model behind the key/value control. The control holds rows, not the
// map, because a row just added has no key and a map cannot hold it.

/** kvObject is the map the rows describe: unnamed rows are left out. */
export function kvObject(rows: readonly [string, string][]): Record<string, string> {
  const obj: Record<string, string> = {};
  for (const [k, v] of rows) if (k.trim() !== "") obj[k] = v;
  return obj;
}

/** kvRows returns `rows` unchanged while they still describe `value` (drafts
 *  included), and fresh rows from `value` once it has changed from outside —
 *  cleared, or a different definition loaded. */
export function kvRows(rows: [string, string][], value: Record<string, string>): [string, string][] {
  const same = JSON.stringify(kvObject(rows)) === JSON.stringify(value);
  return same ? rows : Object.entries(value);
}
