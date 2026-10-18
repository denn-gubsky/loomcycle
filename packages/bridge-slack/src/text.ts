// Outbound text. Every field the bridge fills from text it did not write (the
// agent's answer, a question and its options, a display name) is made inert
// by construction, never by recognising particular markup: a pattern the
// bridge does not know is a pattern it would let through.
//
// Two kinds of field, one rule each, never both on one field:
//
// - Markdown fields (`markdown_text`, `markdown` blocks): the agent writes
//   Markdown, so its formatting is kept, but every character that can OPEN a
//   link, a mention or an entity gets a word joiner (U+2060) after it. The
//   joiner does not show, and with one inside it no parser (Slack's mrkdwn,
//   Slack's Markdown, CommonMark) reads `<!here>`, `](` or `&lt;` as markup.
//   A link the agent writes therefore shows its target as text, which Slack
//   links by itself, so the shown text is always the target.
// - mrkdwn fields (a message's `text` fallback, an ephemeral reply): `&`,
//   `<` and `>` are escaped, `&` first, as Slack's formatting rules ask, so
//   all of it shows as written. The bridge adds no markup of its own there.
//
// The bridge never sets `link_names`.

/** The longest message the bridge sends, in characters. `markdown_text` is
 *  capped at 12,000; this leaves room for Slack counting differently from
 *  JavaScript (UTF-16 units), for word joiners, and for the core's notices
 *  appended to a turn. */
export const MAX_MESSAGE_CHARS = 10_000;

const WJ = "\u2060";

/** A bare @here, @channel or @everyone: with link_names, a broadcast. */
function defuseBare(text: string): string {
  return text.replace(/@(?=(here|channel|everyone)\b)/gi, `@${WJ}`);
}

/** In prose, every "<", "&" and "]" is broken: nothing that starts with one
 *  can be markup. */
function defuseProse(text: string): string {
  return defuseBare(text.replace(/([<&\]])(?!\u2060)/g, `$1${WJ}`));
}

/** In a fenced code block the text stays copyable: only what could still
 *  form a link or a mention, if a parser disagreed that this is code, is
 *  broken. That is "<" before "!", "@", "#" or a URL scheme; "](" and "]:"
 *  followed by space (an inline link, a definition); an entity; a bare
 *  @here. `a < b`, `x && y`, `c[i][j]` and `<T>` are left alone. */
function defuseCode(text: string): string {
  return defuseBare(
    text
      .replace(/<(?=[!@#]|[a-z][\w+.-]*:)/gi, `<${WJ}`)
      .replace(/\](?=\(|:\s)/g, `]${WJ}`)
      .replace(/&(?=#|[a-z]+;)/gi, `&${WJ}`),
  );
}

/** A code fence the bridge recognises: three backticks at the start of the
 *  line and an optional language. Anything else is prose, which is broken
 *  the more thoroughly, so a fence this misses costs looks, not safety. */
const FENCE = /^```[\w+-]*\r?$/;

/** What may continue into markup in the next delta, held back until it
 *  arrives (a stream only). */
const HOLD_PROSE = /@[A-Za-z]{0,8}$/;
const HOLD_CODE = /(<[\w+.-]*|\]:?|&#?[a-z0-9]*|@[A-Za-z]{0,8})$/i;

/** MarkdownDefuser makes Markdown inert as it arrives, for a stream: text is
 *  pushed in deltas and comes out ready to send, with any tail that could
 *  still turn into markup held for the next push. It keeps the one piece of
 *  state that crosses deltas: whether a fenced code block is open. */
export class MarkdownDefuser {
  private inFence = false;
  private inLine = false;
  private lineIsCode = false;
  private held = "";

  /** push returns the safe text for everything pushed so far, except what
   *  it holds; with final, it holds nothing. */
  push(text: string, final = false): string {
    let buf = this.held + text;
    this.held = "";
    let out = "";
    while (buf) {
      const nl = buf.indexOf("\n");
      if (!this.inLine) {
        const line = nl >= 0 ? buf.slice(0, nl) : buf;
        // A line starting with a backtick may be a fence; that is known
        // only once the line is whole.
        if (nl < 0 && !final && line.startsWith("`")) {
          this.held = buf;
          return out;
        }
        const fence = FENCE.test(line);
        this.lineIsCode = fence || this.inFence;
        if (fence) this.inFence = !this.inFence;
        this.inLine = true;
      }
      let seg = nl >= 0 ? buf.slice(0, nl + 1) : buf;
      buf = nl >= 0 ? buf.slice(nl + 1) : "";
      if (nl < 0 && !final) {
        const m = (this.lineIsCode ? HOLD_CODE : HOLD_PROSE).exec(seg);
        if (m) {
          this.held = seg.slice(m.index);
          seg = seg.slice(0, m.index);
        }
      }
      out += this.lineIsCode ? defuseCode(seg) : defuseProse(seg);
      if (nl >= 0) this.inLine = false;
    }
    return out;
  }
}

/** neutralise makes one whole piece of Markdown inert. Idempotent. */
export function neutralise(text: string): string {
  return new MarkdownDefuser().push(text, true);
}

/** escapeMrkdwn makes text safe for a field Slack parses as mrkdwn: `&`
 *  first, then `<` and `>`, so `<!channel>` and `&lt;!here&gt;` both show
 *  as written; and bare mentions broken. */
export function escapeMrkdwn(text: string): string {
  return defuseBare(text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"));
}
