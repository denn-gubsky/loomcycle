import { useEffect, useRef, useState, type ReactNode } from "react";

// MermaidDiagram lazily renders a Mermaid definition to SVG. Theme is read from
// the nearest [data-theme] ancestor (the explorer root / host <html>), defaulting
// to the explorer's dark palette. It renders with securityLevel:"strict" (bundled
// DOMPurify) — the same posture as the runtime's TeamsView diagram. mermaid is an
// OPTIONAL peer dep, so a host without it (or a definition that fails to parse)
// degrades gracefully to the raw code, and a bad diagram never breaks the view.
// Shared by the Markdown ```mermaid fence (RFC BN P2) and the cross-reference
// relationship graph (P4).

// mermaidSeq gives each render a unique DOM id (mermaid.render requires one).
// A module counter is fine in the browser (this never runs in the workflow JS
// sandbox where Math.random / new Date are disallowed).
let mermaidSeq = 0;

export default function MermaidDiagram({ code }: { code: string }): ReactNode {
  const ref = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<{ svg?: string; err?: boolean }>({});

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const mermaid = (await import("mermaid")).default;
        const attr = ref.current?.closest("[data-theme]")?.getAttribute("data-theme");
        const dark = attr ? attr === "dark" : true; // explorer default is dark
        mermaid.initialize({ startOnLoad: false, theme: dark ? "dark" : "default", securityLevel: "strict" });
        const { svg } = await mermaid.render(`lc-mmd-${mermaidSeq++}`, code);
        if (!cancelled) setState({ svg });
      } catch {
        if (!cancelled) setState({ err: true });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [code]);

  if (state.err) {
    // No ref here: the effect already captured the theme on the first (loading)
    // render, and a <pre> ref would conflict with the <div> ref type below.
    return (
      <pre className="md-pre md-mermaid-error" title="diagram failed to render">
        <code>{code}</code>
      </pre>
    );
  }
  if (state.svg) {
    return <div ref={ref} className="md-mermaid" dangerouslySetInnerHTML={{ __html: state.svg }} />;
  }
  return (
    <div ref={ref} className="md-mermaid md-mermaid-loading">
      rendering diagram…
    </div>
  );
}
