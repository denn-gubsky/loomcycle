import { useEffect, useState, type RefObject } from "react";

// useNarrow reports whether the element is narrower than `below` px, and
// follows it as it is resized. The explorer sizes itself to its host's slot,
// not to the window, so this watches the element rather than a media query.
// Without ResizeObserver (a test DOM) it reads the width once.
export function useNarrow(ref: RefObject<HTMLElement | null>, below: number): boolean {
  const [narrow, setNarrow] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = (w: number) => setNarrow(w > 0 && w < below);
    measure(el.getBoundingClientRect().width);
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) measure(e.contentRect.width);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, below]);
  return narrow;
}
