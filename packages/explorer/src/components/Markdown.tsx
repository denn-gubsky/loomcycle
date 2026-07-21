import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";
import MermaidDiagram from "./Mermaid";

// Markdown renders a chunk body as GitHub-flavored Markdown (RFC BN): headings,
// paragraphs, inline styles, links, fenced code, blockquotes, lists, TABLES, and
// ```mermaid diagrams. It uses react-markdown + remark-gfm; react-markdown is
// safe by default (it does not emit raw HTML — no rehype-raw — and its default
// urlTransform strips javascript:/other dangerous hrefs). ```mermaid fences
// render through the shared MermaidDiagram (lazy, securityLevel:"strict",
// graceful fallback to a code block).

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
