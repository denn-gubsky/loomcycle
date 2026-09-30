// A restore's `restored` map carries every counter by name, including the ones
// that count rows the restore HELD BACK: a refused definition or active pointer
// (`*_refused`), or a trigger written disabled because the snapshot stripped
// its literal credentials. Shown under "restored" those read as successes, so
// they get their own line. The CLI (`loomcycle restore`) splits the map the
// same way.

export interface RestoreSummary {
  // The counts of rows written, as one line.
  restored: string;
  // The held-back counts above zero, as one line; absent when there are none.
  notRestored?: string;
}

export function isHeldBack(key: string): boolean {
  return key.endsWith("_refused") || key === "defs_disabled_for_credentials";
}

export function summarizeRestore(counts: Record<string, number>): RestoreSummary {
  const written: string[] = [];
  const held: string[] = [];
  for (const k of Object.keys(counts).sort()) {
    const n = counts[k];
    if (!n || n <= 0) continue;
    (isHeldBack(k) ? held : written).push(`${k}=${n}`);
  }
  let restored: string;
  if (written.length > 0) {
    restored = `restored: ${written.join(", ")}`;
  } else if (held.length > 0) {
    restored = "restored: no new rows";
  } else {
    restored = "restored (0 new rows — every section was already in the store)";
  }
  return held.length > 0 ? { restored, notRestored: `not restored: ${held.join(", ")}` } : { restored };
}
