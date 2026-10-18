// The core's question and review cards as Telegram messages with an inline
// keyboard. A button's callback_data is the core's short button id, never the
// action: Telegram allows 64 bytes, and the core looks the action up among the
// buttons of the card it drew, so a forged id does nothing.

import { VERB, type CanonicalCard, type CardInput } from "@loomcycle/bridge-core";

import { escapeHtml, markdownToHtml } from "./format.js";

/** Telegram's limit for an inline button's callback_data. */
export const MAX_CALLBACK_DATA_BYTES = 64;

export interface InlineKeyboardButton {
  text: string;
  callback_data: string;
}

export interface InlineKeyboardMarkup {
  inline_keyboard: InlineKeyboardButton[][];
  /** Opens the reply box, so a free-text answer is one tap away. */
  force_reply?: boolean;
}

/** A message ready to send: HTML, the plain text to fall back on if Telegram
 *  refuses the HTML, and the keyboard. */
export interface RenderedMessage {
  html: string;
  plain: string;
  replyMarkup?: InlineKeyboardMarkup;
}

export const FREE_TEXT_HINT = "Reply to this message with your answer.";
export const REVIEW_TEXT = "The agent's answer is waiting for your review.";

function button(label: string, id: string): InlineKeyboardButton {
  const bytes = Buffer.byteLength(id, "utf8");
  if (bytes < 1 || bytes > MAX_CALLBACK_DATA_BYTES) {
    // A card whose button cannot be pressed would leave its question open
    // until the run is swept; fail where it is visible instead.
    throw new Error(`button id is ${bytes} bytes; Telegram allows 1 to ${MAX_CALLBACK_DATA_BYTES}`);
  }
  return { text: label, callback_data: id };
}

export function renderCard(card: CanonicalCard): RenderedMessage {
  switch (card.kind) {
    case "question": {
      // Telegram has no text field on a card. A free-text question is answered
      // by replying, which the core takes as the owner's answer, so its "send"
      // button is not drawn.
      const freeText = card.options.length === 0;
      const buttons = card.buttons.filter((b) => !(freeText && b.action.verb === VERB.answer));
      const parts = [card.question, card.context].filter((p): p is string => !!p);
      const html = [...parts.map(markdownToHtml), ...(freeText ? [`<i>${escapeHtml(FREE_TEXT_HINT)}</i>`] : [])].join("\n\n");
      const plain = [...parts, ...(freeText ? [FREE_TEXT_HINT] : [])].join("\n\n");
      return {
        html,
        plain,
        replyMarkup: {
          inline_keyboard: buttons.map((b) => [button(b.label, b.id)]),
          ...(freeText ? { force_reply: true } : {}),
        },
      };
    }
    case "review": {
      const text = card.round ? `${REVIEW_TEXT} (round ${card.round})` : REVIEW_TEXT;
      return {
        html: escapeHtml(text),
        plain: text,
        replyMarkup: { inline_keyboard: [card.buttons.map((b) => button(b.label, b.id))] },
      };
    }
    case "result":
      return { html: markdownToHtml(card.message), plain: card.message };
  }
}

/** parseCallbackData reads a pressed button: the button id the bridge drew,
 *  or nothing for data that cannot be one. */
export function parseCallbackData(data: unknown): CardInput | undefined {
  if (typeof data !== "string") return undefined;
  const bytes = Buffer.byteLength(data, "utf8");
  if (bytes < 1 || bytes > MAX_CALLBACK_DATA_BYTES) return undefined;
  return { buttonId: data };
}
