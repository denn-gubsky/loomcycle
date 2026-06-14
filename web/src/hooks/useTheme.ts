import { useCallback, useEffect, useState } from "react";

// Theme resolution (design-system light/dark).
//
//   effective theme = localStorage override ?? OS prefers-color-scheme
//
// The initial value is applied BEFORE React mounts by the inline script in
// index.html (no flash-of-wrong-theme); this hook mirrors that state, keeps it
// in sync with live OS changes (only while there's no explicit override), and
// exposes a toggle that persists the override.

export type Theme = "light" | "dark";

const STORAGE_KEY = "loomcycle.theme";

function osTheme(): Theme {
  return typeof window !== "undefined" &&
    window.matchMedia &&
    window.matchMedia("(prefers-color-scheme: light)").matches
    ? "light"
    : "dark";
}

function storedOverride(): Theme | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return v === "light" || v === "dark" ? v : null;
  } catch {
    return null;
  }
}

export function resolveTheme(): Theme {
  return storedOverride() ?? osTheme();
}

function applyTheme(t: Theme): void {
  document.documentElement.setAttribute("data-theme", t);
  const meta = document.querySelector('meta[name="color-scheme"]');
  if (meta) meta.setAttribute("content", t);
}

export function useTheme(): {
  theme: Theme;
  setTheme: (t: Theme) => void;
  toggle: () => void;
} {
  const [theme, setThemeState] = useState<Theme>(() => resolveTheme());

  // Live-follow the OS setting, but only while the user hasn't pinned a choice.
  useEffect(() => {
    if (!window.matchMedia) return;
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const onChange = () => {
      if (storedOverride()) return; // an explicit override wins over the OS
      const t = osTheme();
      applyTheme(t);
      setThemeState(t);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const setTheme = useCallback((t: Theme) => {
    try {
      localStorage.setItem(STORAGE_KEY, t);
    } catch {
      /* private mode / disabled storage — fall back to in-memory only */
    }
    applyTheme(t);
    setThemeState(t);
  }, []);

  const toggle = useCallback(() => {
    setTheme(theme === "dark" ? "light" : "dark");
  }, [theme, setTheme]);

  return { theme, setTheme, toggle };
}
