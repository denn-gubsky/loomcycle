import { type ReactNode } from "react";

// Markdown is a minimal, dependency-free, safe-by-construction renderer: it
// builds React elements (never dangerouslySetInnerHTML), so a chunk body —
// which originates from agents and humans — cannot inject markup or script.
// It covers the common block constructs (headings, paragraphs, fenced code,
// blockquotes, ordered/unordered lists) and inline code/bold/italic/links;
// anything unrecognized renders as escaped text. A viewer-grade subset of
// CommonMark (RFC AM Phase 2), not a full implementation.

export default function Markdown({ source }: { source: string }) {
  return <div className="md">{renderBlocks(source)}</div>;
}

function renderBlocks(src: string): ReactNode[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  const out: ReactNode[] = [];
  let i = 0;
  let k = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === "") {
      i++;
      continue;
    }
    // Fenced code block.
    if (line.startsWith("```")) {
      const buf: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) {
        buf.push(lines[i]);
        i++;
      }
      i++; // consume the closing fence (if present)
      out.push(
        <pre key={k++} className="md-pre">
          <code>{buf.join("\n")}</code>
        </pre>,
      );
      continue;
    }
    // Heading.
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      out.push(heading(h[1].length, inline(h[2]), k++));
      i++;
      continue;
    }
    // Blockquote (consecutive `> ` lines).
    if (/^>\s?/.test(line)) {
      const buf: string[] = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        buf.push(lines[i].replace(/^>\s?/, ""));
        i++;
      }
      out.push(
        <blockquote key={k++}>{inline(buf.join(" "))}</blockquote>,
      );
      continue;
    }
    // Unordered / ordered list (consecutive item lines).
    const ul = /^[-*]\s+(.*)$/;
    const ol = /^\d+\.\s+(.*)$/;
    if (ul.test(line) || ol.test(line)) {
      const ordered = ol.test(line);
      const re = ordered ? ol : ul;
      const items: ReactNode[] = [];
      let j = 0;
      while (i < lines.length && re.test(lines[i])) {
        const m = re.exec(lines[i])!;
        items.push(<li key={j++}>{inline(m[1])}</li>);
        i++;
      }
      out.push(ordered ? <ol key={k++}>{items}</ol> : <ul key={k++}>{items}</ul>);
      continue;
    }
    // Paragraph: gather consecutive non-blank, non-special lines.
    const buf: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() !== "" &&
      !lines[i].startsWith("```") &&
      !/^(#{1,6})\s+/.test(lines[i]) &&
      !/^>\s?/.test(lines[i]) &&
      !ul.test(lines[i]) &&
      !ol.test(lines[i])
    ) {
      buf.push(lines[i]);
      i++;
    }
    out.push(<p key={k++}>{inline(buf.join(" "))}</p>);
  }
  return out;
}

function heading(level: number, children: ReactNode, key: number): ReactNode {
  switch (level) {
    case 1:
      return <h1 key={key}>{children}</h1>;
    case 2:
      return <h2 key={key}>{children}</h2>;
    case 3:
      return <h3 key={key}>{children}</h3>;
    case 4:
      return <h4 key={key}>{children}</h4>;
    case 5:
      return <h5 key={key}>{children}</h5>;
    default:
      return <h6 key={key}>{children}</h6>;
  }
}

// inline tokenizes a single line into code spans, bold, italic, and links.
// Links render as an anchor only for http(s)/relative URLs (a javascript: or
// other scheme renders as plain label text — React does not sanitize href).
const INLINE_RE = /(`[^`]+`)|(\*\*[^*]+?\*\*)|(\*[^*]+?\*)|(\[[^\]]+\]\([^)]+\))/;

function inline(text: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  let rest = text;
  let key = 0;
  while (rest.length > 0) {
    const m = INLINE_RE.exec(rest);
    if (!m) {
      nodes.push(rest);
      break;
    }
    if (m.index > 0) nodes.push(rest.slice(0, m.index));
    const tok = m[0];
    if (tok.startsWith("`")) {
      nodes.push(<code key={key++}>{tok.slice(1, -1)}</code>);
    } else if (tok.startsWith("**")) {
      nodes.push(<strong key={key++}>{tok.slice(2, -2)}</strong>);
    } else if (tok.startsWith("*")) {
      nodes.push(<em key={key++}>{tok.slice(1, -1)}</em>);
    } else {
      const lm = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(tok)!;
      const label = lm[1];
      const url = lm[2];
      // Anchor only for http(s) or a same-origin relative path. Exclude
      // protocol-relative `//host` (which `startsWith("/")` would otherwise
      // accept) — it resolves to an off-site URL, not a relative path.
      const relative = url.startsWith("/") && !url.startsWith("//");
      if (/^https?:\/\//i.test(url) || relative) {
        nodes.push(
          <a key={key++} href={url} target="_blank" rel="noopener noreferrer">
            {label}
          </a>,
        );
      } else {
        nodes.push(label); // unsafe scheme → plain text
      }
    }
    rest = rest.slice(m.index + tok.length);
  }
  return nodes;
}
