// Markdown, as an agent writes it, to the HTML subset Telegram accepts
// (parse_mode "HTML"): bold, italic, strikethrough, inline code, code blocks
// and links. Everything else is text, with <, > and & escaped, so an agent's
// output can never inject a tag.
//
// Telegram rejects a whole message whose HTML it cannot parse, so the output
// must always be well formed, even for half-written Markdown mid-stream:
//   - an unclosed ``` fence runs to the end, as a code block;
//   - an unmatched marker stays a literal character;
//   - an emphasis span never contains a tag, so spans nest and never cross;
//   - code is never inside another entity, which Telegram forbids.
// If Telegram refuses it anyway, the port resends the message as plain text.

// Placeholders for code spans and links while the rest is escaped. Stripped
// from the input first, so the input cannot forge one.
const CODE = "\u0001";
const LINK = "\u0002";
const PLACEHOLDERS = /[\u0001\u0002]/g;

export function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function escapeAttr(s: string): string {
  return escapeHtml(s).replace(/"/g, "&quot;");
}

const FENCE_OPEN = /^\s*```\s*([A-Za-z0-9_+#.-]*)\s*$/;
const FENCE_CLOSE = /^\s*```\s*$/;
const HEADING = /^#{1,6}\s+(.+?)\s*#*\s*$/;

/** markdownToHtml converts an agent's Markdown to Telegram HTML. */
export function markdownToHtml(md: string): string {
  const lines = md.replace(PLACEHOLDERS, "").split("\n");
  const out: string[] = [];
  let prose: string[] = [];
  const flush = () => {
    if (prose.length > 0) out.push(inline(prose.join("\n")));
    prose = [];
  };
  for (let i = 0; i < lines.length; i++) {
    const fence = FENCE_OPEN.exec(lines[i]!);
    if (!fence) {
      const h = HEADING.exec(lines[i]!);
      // A heading shows as a bold line; one with its own asterisks stays as written.
      prose.push(h && !h[1]!.includes("*") ? `**${h[1]}**` : lines[i]!);
      continue;
    }
    flush();
    const body: string[] = [];
    for (i++; i < lines.length && !FENCE_CLOSE.test(lines[i]!); i++) body.push(lines[i]!);
    if (body.length === 0) continue;
    const code = escapeHtml(body.join("\n"));
    const lang = fence[1];
    out.push(lang ? `<pre><code class="language-${escapeAttr(lang)}">${code}</code></pre>` : `<pre>${code}</pre>`);
  }
  flush();
  return out.join("\n");
}

const CODE_SPAN = /```([^`]+?)```|``([^`]+?)``|`([^`\n]+?)`/g;
// Only schemes Telegram opens; anything else stays text.
const LINK_SPAN = /\[([^[\]\n]+)\]\(\s*((?:https?:\/\/|tg:\/\/|mailto:)[^\s()<>]+)\s*\)/g;

function inline(text: string): string {
  const codes: string[] = [];
  const links: string[] = [];
  let s = text.replace(CODE_SPAN, (_m, a?: string, b?: string, c?: string) => {
    codes.push((a ?? b ?? c)!);
    return `${CODE}${codes.length - 1}${CODE}`;
  });
  s = s.replace(LINK_SPAN, (_m, label: string, url: string) => {
    // A link may hold emphasis but not code: its code spans become plain text.
    const plain = label.replace(/\u0001(\d+)\u0001/g, (_c, n: string) => codes[Number(n)]!);
    links.push(`<a href="${escapeAttr(url)}">${emphasis(escapeHtml(plain))}</a>`);
    return `${LINK}${links.length - 1}${LINK}`;
  });
  s = emphasis(escapeHtml(s));
  return s
    .replace(/\u0002(\d+)\u0002/g, (_m, n: string) => links[Number(n)]!)
    .replace(/\u0001(\d+)\u0001/g, (_m, n: string) => `<code>${escapeHtml(codes[Number(n)]!)}</code>`);
}

// Each span's content excludes "<" and ">" (so it holds no tag: the text is
// already escaped, so these only come from tags made here) and the code
// placeholder (Telegram allows no entity around code).
const BOLD_STARS = /\*\*(?=[^\s*])([^\u0001<>]*?[^\s*])\*\*/g;
const BOLD_UNDERSCORES = /(^|[^\w])__(?=[^\s_])([^\u0001<>]*?[^\s_])__(?!\w)/g;
const STRIKE = /~~(?=[^\s~])([^\u0001<>]*?[^\s~])~~/g;
// Single markers must hug a word on both sides: "* item" and snake_case are text.
const ITALIC_STAR = /(^|[^\w*])\*(?=[^\s*])([^\u0001<>*\n]*?[^\s*])\*(?![\w*])/g;
const ITALIC_UNDERSCORE = /(^|[^\w])_(?=[^\s_])([^\u0001<>_\n]*?[^\s_])_(?!\w)/g;

function emphasis(s: string): string {
  return s
    .replace(BOLD_STARS, "<b>$1</b>")
    .replace(BOLD_UNDERSCORES, "$1<b>$2</b>")
    .replace(STRIKE, "<s>$1</s>")
    .replace(ITALIC_STAR, "$1<i>$2</i>")
    .replace(ITALIC_UNDERSCORE, "$1<i>$2</i>");
}
