// Block Kit rendering of the core's cards. Text goes in `markdown` blocks,
// which take standard Markdown, so the agent's question needs no conversion
// to Slack's mrkdwn. Each button's value is its short id; the bridge looks the
// action up in the conversation that drew the card.

import { VERB, type CanonicalCard, type CardButton } from "@loomcycle/bridge-core";

import { CARD_BLOCK_ID, ACTION_ID_PREFIX } from "./inbound.js";
import type { ChannelType } from "./identity.js";
import { neutralise } from "./text.js";

export type Block = Record<string, unknown>;

export interface RenderedCard {
  /** The notification and accessibility fallback. */
  text: string;
  blocks: Block[];
}

/** Button labels are capped at 75 characters by Block Kit. */
export const MAX_BUTTON_LABEL = 75;
/** Slack caps all markdown blocks of one message at 12,000 characters together;
 *  the question and its context get a share each, well clear of it. */
export const MAX_QUESTION_CHARS = 3000;
export const MAX_CONTEXT_CHARS = 3000;

function clip(s: string, max: number): string {
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`;
}

function md(text: string): Block {
  return { type: "markdown", text: neutralise(text) };
}

function button(b: CardButton): Block {
  const style = b.action.verb === VERB.approve ? { style: "primary" } : {};
  return {
    type: "button",
    action_id: `${ACTION_ID_PREFIX}${b.id}`,
    value: b.id,
    text: { type: "plain_text", text: clip(b.label, MAX_BUTTON_LABEL), emoji: false },
    ...style,
  };
}

/** A free-text question's "send" button carries no answer; Slack messages
 *  have no text field to send, so the person replies in the conversation
 *  instead and the core takes the owner's reply as the answer. */
function isSendButton(b: CardButton): boolean {
  return b.action.verb === VERB.answer && !b.action.answer;
}

export function renderCard(card: CanonicalCard, channelType: ChannelType): RenderedCard {
  if (card.kind === "result") {
    return { text: clip(card.message, 300), blocks: [md(card.message)] };
  }
  const actions = (buttons: CardButton[]): Block => ({
    type: "actions",
    block_id: CARD_BLOCK_ID[channelType],
    elements: buttons.map(button),
  });

  if (card.kind === "review") {
    const title =
      card.round && card.round > 1 ? `The revised answer (round ${card.round}) is ready for review.` : "The answer is ready for review.";
    return { text: title, blocks: [md(`**${title}**`), actions(card.buttons)] };
  }

  const question = clip(card.question.trim() || "The agent has a question.", MAX_QUESTION_CHARS);
  const blocks: Block[] = [md(question)];
  if (card.context?.trim()) blocks.push(md(clip(card.context.trim(), MAX_CONTEXT_CHARS)));
  // A long option shows whole in the card's text, since its button is cut short.
  if (card.options.some((o) => o.length > MAX_BUTTON_LABEL)) {
    blocks.push(md(card.options.map((o, i) => `${i + 1}. ${o}`).join("\n")));
  }
  const buttons = card.buttons.filter((b) => !isSendButton(b));
  if (card.options.length === 0) {
    const where = channelType === "im" ? "Reply with your answer" : "Reply in this thread with your answer";
    blocks.push(md(`_${where}, or decline._`));
  }
  blocks.push(actions(buttons));
  return { text: clip(question, 300), blocks };
}
