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

// flowchartNode finds the flowchart node a click landed in and returns the id it
// was DECLARED with. Mermaid wraps that id in its own (`flowchart-<id>-<n>`,
// behind the render id in newer versions), so it is matched from the right. A
// click outside any node — or a diagram that is not a flowchart — is undefined.
export function flowchartNode(target: Element | null): string | undefined {
  const node = target?.closest("g.node");
  return node ? /flowchart-(.+)-\d+$/.exec(node.id)?.[1] : undefined;
}

export default function MermaidDiagram({
  code,
  onNodeClick,
}: {
  code: string;
  // Called with a flowchart node's declared id when it is clicked. Makes the
  // nodes read as clickable; omit for a diagram that is only a picture.
  onNodeClick?: (nodeId: string) => void;
}): ReactNode {
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
    return (
      <div
        ref={ref}
        className={onNodeClick ? "md-mermaid md-mermaid-nav" : "md-mermaid"}
        onClick={
          onNodeClick &&
          ((e) => {
            const id = flowchartNode(e.target as Element);
            if (id) onNodeClick(id);
          })
        }
        dangerouslySetInnerHTML={{ __html: state.svg }}
      />
    );
  }
  return (
    <div ref={ref} className="md-mermaid md-mermaid-loading">
      rendering diagram…
    </div>
  );
}
