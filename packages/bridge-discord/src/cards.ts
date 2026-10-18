// The core's question and review cards as Discord messages: an embed with the
// text, and rows of buttons whose custom_id is the core's short button id.
// Discord keeps a button's custom_id to 100 characters, and the core looks the
// action up by id, so the id is all a button carries.

import { VERB, type CanonicalCard, type CardButton } from "@loomcycle/bridge-core";

export interface MessageBody {
  content?: string;
  embeds?: Array<{ title?: string; description?: string; footer?: { text: string } }>;
  components?: Array<{ type: 1; components: Button[] }>;
  allowed_mentions: { parse: string[] };
  flags?: number;
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
const MAX_DESCRIPTION = 4096;
const BUTTONS_PER_ROW = 5;
const Style = { PRIMARY: 1, SECONDARY: 2, SUCCESS: 3, DANGER: 4 } as const;

/** Agent text may name @everyone or a role: nothing the bridge posts pings. */
export const NO_MENTIONS = { parse: [] as string[] };

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

export function textMessage(text: string): MessageBody {
  return { content: text, allowed_mentions: NO_MENTIONS };
}

export function renderCard(card: CanonicalCard): MessageBody {
  switch (card.kind) {
    case "result":
      // Replaces a used card: the buttons go, so it cannot be answered twice.
      return { content: card.message, embeds: [], components: [], allowed_mentions: NO_MENTIONS };
    case "question": {
      // A Discord message has no text field, so a free-text question is
      // answered with the owner's next /ask; the core's "Send" button would
      // only ever say "type an answer first".
      const freeText = card.options.length === 0;
      const buttons = card.buttons
        .filter((b) => !(freeText && b.action.verb === VERB.answer))
        .map((b) => button(b, b.action.verb === VERB.decline ? Style.SECONDARY : Style.PRIMARY));
      const description = [card.question, card.context, freeText ? FREE_TEXT_HINT : undefined].filter(Boolean).join("\n\n");
      return {
        embeds: [{ title: "Question", description: clip(description || "The agent has a question.", MAX_DESCRIPTION) }],
        components: rows(buttons),
        allowed_mentions: NO_MENTIONS,
      };
    }
    case "review": {
      const buttons = card.buttons.map((b) => button(b, b.action.verb === VERB.approve ? Style.SUCCESS : Style.DANGER));
      return {
        embeds: [
          {
            title: "Review",
            description: "The agent's answer is waiting for your review.",
            ...(card.round ? { footer: { text: `Round ${card.round}` } } : {}),
          },
        ],
        components: rows(buttons),
        allowed_mentions: NO_MENTIONS,
      };
    }
  }
}
