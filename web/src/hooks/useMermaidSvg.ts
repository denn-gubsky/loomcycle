import { useEffect, useRef, useState } from "react";
import type { Theme } from "./useTheme";

// useMermaidSvg renders Mermaid source to an SVG string in-page. mermaid is
// lazy-imported (a big dep, kept out of the main bundle) and initialized per
// the app theme so edges and labels stay legible on light or dark. A render
// failure clears the SVG and reports the error, so the caller can fall back to
// showing the source. A stale render never wins a race: each new source
// cancels the one before it.
//
// The SVG is meant for dangerouslySetInnerHTML. That is safe only because the
// source is server-generated and sanitized (teamgraph's render strips
// newlines, -->, and %% from ids and labels) AND mermaid renders it with
// securityLevel "strict" (its bundled DOMPurify). Keep both.
export function useMermaidSvg(
  source: string | null | undefined,
  theme: Theme,
  idPrefix = "mmd",
): { svg: string; renderErr: string } {
  const [svg, setSvg] = useState<string>("");
  const [renderErr, setRenderErr] = useState<string>("");
  // mermaid.render needs a document-unique element id per call.
  const idRef = useRef(0);

  useEffect(() => {
    if (!source) {
      setSvg("");
      return;
    }
    let cancelled = false;
    setRenderErr("");
    void (async () => {
      try {
        const mermaid = (await import("mermaid")).default;
        mermaid.initialize({
          startOnLoad: false,
          theme: theme === "dark" ? "dark" : "default",
          securityLevel: "strict",
        });
        const id = `${idPrefix}-${++idRef.current}`;
        const out = await mermaid.render(id, source);
        if (!cancelled) setSvg(out.svg);
      } catch (e) {
        if (!cancelled) {
          setSvg("");
          setRenderErr(e instanceof Error ? e.message : String(e));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [source, theme, idPrefix]);

  return { svg, renderErr };
}
