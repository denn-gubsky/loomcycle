// Outbound text. Slack's Web API takes standard Markdown in `markdown_text`
// and in `markdown` blocks, so the agent's Markdown is sent as written.

/** The longest message the bridge sends, in characters. `markdown_text` is
 *  capped at 12,000; this leaves room for Slack counting differently from
 *  JavaScript (UTF-16 units) and for the core's notices appended to a turn. */
export const MAX_MESSAGE_CHARS = 10_000;

/** neutralise defuses Slack's special mentions (`<!channel>`, `<!here>`,
 *  `<!everyone>`, `<!subteam^…>`) in text the agent wrote. A shared chat's
 *  speakers can steer what the agent writes, and must not get it to page a
 *  whole channel. A word joiner after the `<` stops Slack parsing it and does
 *  not show. Idempotent. */
export function neutralise(text: string): string {
  return text.replace(/<!/g, "<\u2060!");
}
