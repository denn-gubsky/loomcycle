// Per-browser view preferences (format, wrap). Storage can be absent or throw
// (private windows, blocked site data), and the editor must work without it.

export function readPref(key: string | undefined): string | null {
  if (!key) return null;
  try {
    return globalThis.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

export function writePref(key: string | undefined, value: string): void {
  if (!key) return;
  try {
    globalThis.localStorage?.setItem(key, value);
  } catch {
    // Not remembered; the choice still applies to this view.
  }
}
