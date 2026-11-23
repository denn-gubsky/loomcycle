import { useCallback, useState } from "react";

// Per-browser layout memory (RFC GZ): which panes are collapsed and which side
// panel is open, kept like the splitter widths. Storage can be missing or
// refuse (a private window, a sandboxed host), so every read falls back to the
// default and every write is best-effort.

export function readStored(key: string): string | null {
  try {
    return globalThis.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

export function writeStored(key: string, value: string | null): void {
  try {
    if (value === null) globalThis.localStorage?.removeItem(key);
    else globalThis.localStorage?.setItem(key, value);
  } catch {
    // Not remembered; the layout still works for this visit.
  }
}

// usePersistedFlag is a boolean kept under `key` ("1" / "0").
export function usePersistedFlag(key: string, fallback = false): [boolean, (v: boolean | ((cur: boolean) => boolean)) => void] {
  const [value, setValue] = useState<boolean>(() => {
    const s = readStored(key);
    return s === null ? fallback : s === "1";
  });
  const set = useCallback(
    (v: boolean | ((cur: boolean) => boolean)) =>
      setValue((cur) => {
        const next = typeof v === "function" ? v(cur) : v;
        writeStored(key, next ? "1" : "0");
        return next;
      }),
    [key],
  );
  return [value, set];
}

// usePersistedChoice is one of `choices` (or null) kept under `key`; a stored
// value that is no longer a choice reads as null.
export function usePersistedChoice<T extends string>(
  key: string,
  choices: readonly T[],
): [T | null, (v: T | null | ((cur: T | null) => T | null)) => void] {
  const [value, setValue] = useState<T | null>(() => {
    const s = readStored(key);
    return s !== null && (choices as readonly string[]).includes(s) ? (s as T) : null;
  });
  const set = useCallback(
    (v: T | null | ((cur: T | null) => T | null)) =>
      setValue((cur) => {
        const next = typeof v === "function" ? v(cur) : v;
        writeStored(key, next);
        return next;
      }),
    [key],
  );
  return [value, set];
}

// isTypingTarget: a key pressed here is text, not a shortcut.
export function isTypingTarget(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el || !el.tagName) return false;
  const tag = el.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable === true;
}

// shortcutApplies: a page-level key belongs to this explorer when the focus is
// inside it or nowhere in particular (the page body), never in a text field or
// a dialog, and never with a modifier other than Shift.
export function shortcutApplies(e: KeyboardEvent, root: HTMLElement | null): boolean {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return false;
  if (isTypingTarget(e.target)) return false;
  const t = e.target as Node | null;
  if (!root) return false;
  if (t && (t as HTMLElement).closest?.(".modal-overlay")) return false;
  return t === document.body || t === document.documentElement || (!!t && root.contains(t));
}
