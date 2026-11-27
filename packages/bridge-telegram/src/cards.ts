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
}

/** Opens the reply box. selective limits it to the user the message replies
 *  to (or mentions). */
export interface ForceReply {
  force_reply: true;
  selective?: boolean;
}

/** A message ready to send: HTML, the plain text to fall back on if Telegram
 *  refuses the HTML, and the keyboard. */
export interface RenderedMessage {
  html: string;
  plain: string;
  replyMarkup?: InlineKeyboardMarkup;
  /** A second message to send after this one, opening the reply box. A
   *  reply_markup is one kind of markup, so the box cannot share the card's
   *  message with its keyboard. */
  replyPrompt?: string;
}

export const FREE_TEXT_HINT = "Reply to this message with your answer.";
/** A free-text question in an untrusted chat: the core refuses a typed answer
 *  from an untrusted speaker, so it can only be declined there. */
export const BUTTONS_ONLY_HINT = "Only the buttons can answer here.";
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

/** Telegram takes 4096 characters of message text; a card keeps well clear. */
export const MAX_CARD_CHARS = 4000;
/** A question is cut to make room for its context, but never below this. */
const MIN_QUESTION_CHARS = 300;

function clip(s: string, max: number): string {
  return s.length <= max ? s : `${s.slice(0, Math.max(0, max - 1))}…`;
}

/** fitCard is a question card's text parts within MAX_CARD_CHARS. The
 *  context ends with the numbered options and how to answer them
 *  (bridge-core, RFC HG), so a long question is cut before it is. The cut is
 *  made in the Markdown, before it becomes HTML: the HTML only drops markup
 *  and its entities count as one character each, so the shown text is never
 *  longer than the Markdown. */
function fitCard(question: string, context: string | undefined, hint: string | undefined): string[] {
  const fixed = hint ? hint.length + 2 : 0;
  const ctx = context ?? "";
  const room = MAX_CARD_CHARS - fixed - (ctx ? ctx.length + 2 : 0);
  const q = clip(question, Math.max(MIN_QUESTION_CHARS, room));
  const c = ctx ? clip(ctx, MAX_CARD_CHARS - fixed - (q ? q.length + 2 : 0)) : "";
  return [q, c].filter((p) => !!p);
}

export interface RenderOptions {
  /** The card is drawn into a chat whose speakers are untrusted. */
  untrusted?: boolean;
}

export function renderCard(card: CanonicalCard, opts: RenderOptions = {}): RenderedMessage {
  switch (card.kind) {
    case "question": {
      // Telegram has no text field on a card. A free-text question is answered
      // by replying, which the core takes as the owner's answer, so its "send"
      // button is not drawn and a prompt that opens the reply box follows.
      const freeText = card.options.length === 0;
      const buttons = card.buttons.filter((b) => !(freeText && b.action.verb === VERB.answer));
      // A reply cannot answer in an untrusted chat, so a reply box would
      // invite a typed answer the core is bound to refuse.
      const replyable = freeText && !opts.untrusted;
      const hint = freeText && !replyable ? BUTTONS_ONLY_HINT : undefined;
      const parts = fitCard(card.question, card.context, hint);
      const html = [...parts.map(markdownToHtml), ...(hint ? [`<i>${escapeHtml(hint)}</i>`] : [])].join("\n\n");
      const plain = [...parts, ...(hint ? [hint] : [])].join("\n\n");
      return {
        html,
        plain,
        replyMarkup: { inline_keyboard: buttons.map((b) => [button(b.label, b.id)]) },
        ...(replyable ? { replyPrompt: FREE_TEXT_HINT } : {}),
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
