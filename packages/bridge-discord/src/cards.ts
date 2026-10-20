// The core's question and review cards as Discord messages: the text, and
// rows of buttons whose custom_id is the core's short button id. Discord keeps
// a button's custom_id to 100 characters, and the core looks the action up by
// id, so the id is all a button carries.
//
// Every message the bridge posts carries agent-written text, which an
// untrusted speaker can steer. So none of them pings anyone (allowed_mentions
// parses nothing) and none shows a link preview: Discord fetches a link to
// build its preview without anyone clicking, which would send the URL's query
// string to whoever runs the server. SUPPRESS_EMBEDS also hides embeds the
// bridge would build, so cards are plain content. And no masked link
// (`[label](url)`) survives: Discord renders one in a bot's message, so the
// agent could show one address and open another.

import { VERB, type CanonicalCard, type CardButton } from "@loomcycle/bridge-core";

export interface MessageBody {
  content?: string;
  components?: Array<{ type: 1; components: Button[] }>;
  allowed_mentions: { parse: string[] };
  flags: number;
}

interface Button {
  type: 2;
  style: number;
  label: string;
  custom_id: string;
}

export const CUSTOM_ID_PREFIX = "lc:";
export const MAX_CUSTOM_ID = 100;
const MAX_LABEL = 80;
export const MAX_CONTENT = 2000;
const BUTTONS_PER_ROW = 5;
const Style = { PRIMARY: 1, SECONDARY: 2, SUCCESS: 3, DANGER: 4 } as const;

export const NO_MENTIONS = { parse: [] as string[] };
export const SUPPRESS_EMBEDS = 1 << 2;

export const FREE_TEXT_HINT = "Answer with `/ask` and your answer, or decline.";

export function customIdOf(buttonId: string): string {
  const id = `${CUSTOM_ID_PREFIX}${buttonId}`;
  if (id.length > MAX_CUSTOM_ID) throw new Error("button id too long for Discord");
  return id;
}

/** parseCustomId returns the core's button id, or undefined when the button
 *  is not one this bridge drew. */
export function parseCustomId(customId: unknown): { buttonId: string } | undefined {
  if (typeof customId !== "string") return undefined;
  const m = /^lc:([A-Za-z0-9_-]{1,64})$/.exec(customId);
  return m ? { buttonId: m[1] } : undefined;
}

function clip(s: string, max: number): string {
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`;
}

function rows(buttons: Button[]): MessageBody["components"] {
  const out: NonNullable<MessageBody["components"]> = [];
  for (let i = 0; i < buttons.length; i += BUTTONS_PER_ROW) out.push({ type: 1, components: buttons.slice(i, i + BUTTONS_PER_ROW) });
  return out;
}

function button(b: CardButton, style: number): Button {
  return { type: 2, style, label: clip(b.label || "…", MAX_LABEL), custom_id: customIdOf(b.id) };
}

const MASKED_LINK = /\[([^[\]\n]*)\]\(\s*<?([^\s()<>]+)>?(?:\s+"[^"\n]*")?\s*\)/g;

/** neutraliseLinks rewrites each masked link as `label (<url>)`, so the shown
 *  text holds the real target (the angle brackets also keep it from
 *  unfurling). A `](` it cannot parse (a URL with parentheses, nested
 *  brackets) gets a zero-width space, which breaks the link and shows it raw. */
export function neutraliseLinks(text: string): string {
  return text.replace(MASKED_LINK, (_, label: string, url: string) => `${label} (<${url}>)`).replace(/\]\(/g, "]\u200b(");
}

/** textMessage is one short message: a line from the bridge, or a piece of
 *  a turn already cut to size (neutralising may lengthen it, hence the clip). */
export function textMessage(text: string): MessageBody {
  return { content: clip(neutraliseLinks(text), MAX_CONTENT), allowed_mentions: NO_MENTIONS, flags: SUPPRESS_EMBEDS };
}

export function renderCard(card: CanonicalCard): MessageBody {
  switch (card.kind) {
    case "result":
      // Replaces a used card: the buttons go, so it cannot be answered twice.
      return { ...textMessage(card.message), components: [] };
    case "question": {
      // A Discord message has no text field, so a free-text question is
      // answered with the owner's next /ask; the core's "Send" button would
      // only ever say "type an answer first".
      const freeText = card.options.length === 0;
      const buttons = card.buttons
        .filter((b) => !(freeText && b.action.verb === VERB.answer))
        .map((b) => button(b, b.action.verb === VERB.decline ? Style.SECONDARY : Style.PRIMARY));
      const hint = freeText ? `\n\n${FREE_TEXT_HINT}` : "";
      const text = neutraliseLinks([card.question || "The agent has a question.", card.context].filter(Boolean).join("\n\n"));
      return {
        content: `**Question**\n${clip(text, MAX_CONTENT - hint.length - 13)}${hint}`,
        components: rows(buttons),
        allowed_mentions: NO_MENTIONS,
        flags: SUPPRESS_EMBEDS,
      };
    }
    case "review": {
      const buttons = card.buttons.map((b) => button(b, b.action.verb === VERB.approve ? Style.SUCCESS : Style.DANGER));
      return {
        content: `**Review**\nThe agent's answer is waiting for your review.${card.round ? ` Round ${card.round}.` : ""}`,
        components: rows(buttons),
        allowed_mentions: NO_MENTIONS,
        flags: SUPPRESS_EMBEDS,
      };
    }
  }
}
