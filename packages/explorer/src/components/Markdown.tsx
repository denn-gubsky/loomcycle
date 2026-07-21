import { useEffect, useRef, useState, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

// Markdown renders a chunk body as GitHub-flavored Markdown (RFC BN): headings,
// paragraphs, inline styles, links, fenced code, blockquotes, lists, TABLES, and
// ```mermaid diagrams. It uses react-markdown + remark-gfm; react-markdown is
// safe by default (it does not emit raw HTML — no rehype-raw — and its default
// urlTransform strips javascript:/other dangerous hrefs). Mermaid fences render
// through a lazy `import("mermaid")` with securityLevel:"strict" (bundled
// DOMPurify), the same posture as the runtime's TeamsView diagram; mermaid is an
// OPTIONAL peer dep, so a host without it (or a diagram that fails to parse)
// degrades gracefully to a code block.

export default function Markdown({ source }: { source: string }) {
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={MD_COMPONENTS}>
        {source}
      </ReactMarkdown>
    </div>
  );
}

const MD_COMPONENTS: Components = {
  // react-markdown already sanitized the href; add target/rel for external links.
  a({ href, children }) {
    const url = href ?? "";
    const external = /^https?:\/\//i.test(url);
    return (
      <a href={url} target={external ? "_blank" : undefined} rel={external ? "noopener noreferrer" : undefined}>
        {children}
      </a>
    );
  },
  // Let the code renderer own the block wrapper so a mermaid fence isn't wrapped
  // in a <pre> (and a normal code block gets exactly one <pre>).
  pre({ children }) {
    return <>{children}</>;
  },
  code({ className, children }) {
    const lang = /language-(\w+)/.exec(className ?? "")?.[1];
    const raw = String(children ?? "");
    if (lang === "mermaid") {
      return <MermaidDiagram code={raw.replace(/\n$/, "")} />;
    }
    // Fenced block (has a language) or a multi-line block without one → <pre>.
    if (lang || raw.includes("\n")) {
      return (
        <pre className="md-pre">
          <code className={className}>{children}</code>
        </pre>
      );
    }
    return <code>{children}</code>; // inline
  },
};

// mermaidSeq gives each render a unique DOM id (mermaid.render requires one).
// A module counter is fine in the browser (this never runs in the workflow JS
// sandbox where Math.random / new Date are disallowed).
let mermaidSeq = 0;

// MermaidDiagram lazily renders a ```mermaid fence to SVG. Theme is read from the
// nearest [data-theme] ancestor (the explorer root / host <html>), defaulting to
// the explorer's dark palette. A parse error or a missing mermaid dep falls back
// to the raw code so a bad diagram never breaks the document view.
function MermaidDiagram({ code }: { code: string }): ReactNode {
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
