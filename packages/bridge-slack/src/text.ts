// Outbound text. Slack's Web API takes standard Markdown in `markdown_text`
// and in `markdown` blocks, so the agent's Markdown is sent as written.

/** The longest message the bridge sends, in characters. `markdown_text` is
 *  capped at 12,000; this leaves room for Slack counting differently from
 *  JavaScript (UTF-16 units) and for the core's notices appended to a turn. */
export const MAX_MESSAGE_CHARS = 10_000;

/** A word joiner: invisible, and it splits a token Slack would parse. */
const WJ = "\u2060";

function defuseBare(text: string): string {
  return text.replace(/@(here|channel|everyone)\b/gi, `@${WJ}$1`);
}

/** neutralise defuses every mention in text sent as standard Markdown
 *  (`markdown_text`, a `markdown` block): `<!channel>`, `<!here>`,
 *  `<!everyone>`, `<!subteam^…>`, `<@U…>`, and a bare `@here`, `@channel` or
 *  `@everyone`. The agent's text, a question's options and a display name are
 *  not ours: a shared chat's speakers can steer what the agent writes, and
 *  must not get it to page a channel or a workspace. Slack's docs do not say
 *  whether Markdown fields parse these, so they are broken either way: a word
 *  joiner after the `<` or `@` stops the parse and does not show. Idempotent. */
export function neutralise(text: string): string {
  return defuseBare(text.replace(/<([!@])/g, `<${WJ}$1`));
}

/** escapeMrkdwn makes text safe for a field Slack parses as mrkdwn (a
 *  message's `text`): `&`, `<` and `>` become entities, as Slack's formatting
 *  rules ask, so `<!channel>` arrives as literal text; bare mentions are
 *  broken as in neutralise. The bridge never sets `link_names`. */
export function escapeMrkdwn(text: string): string {
  return defuseBare(text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"));
}
