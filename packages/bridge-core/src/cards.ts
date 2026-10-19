// The two things a loomcycle run can wait on a person for — a question (an
// Interruption) and a held answer (a review) — as platform-neutral cards. A
// bridge renders them in its platform's own format: an Adaptive Card, Block
// Kit, an inline keyboard. A used card is replaced by a result card, so it
// cannot be answered twice.

export const VERB = {
  answer: "lc.answer",
  decline: "lc.decline",
  approve: "lc.approve",
  reject: "lc.reject",
} as const;

export type Verb = (typeof VERB)[keyof typeof VERB];

/** What a card's button or form sends back. */
export interface CardAction {
  verb: Verb;
  runId: string;
  interruptId?: string;
  answer?: string;
  feedback?: string;
}

/** One button of a card. `id` is short and opaque: a platform whose button
 *  payload is small (Telegram allows 64 bytes, Discord 100 characters) sends
 *  the id, and the bridge looks the action up in the conversation that drew
 *  the card. A platform with room may carry the action itself. */
export interface CardButton {
  id: string;
  label: string;
  action: CardAction;
}

export type CanonicalCard =
  | {
      kind: "question";
      runId: string;
      interruptId: string;
      question: string;
      context?: string;
      /** Up to six choices. Empty: the question wants a free-text answer. */
      options: string[];
      /** One per option, or one "send" button for a free-text answer, then decline. */
      buttons: CardButton[];
    }
  | {
      kind: "review";
      runId: string;
      round?: number;
      /** Approve, then send back. */
      buttons: CardButton[];
    }
  | { kind: "result"; message: string };

/** What a platform hands back when a card is used: the action itself, or the
 *  id of a button the bridge drew, with any text the person typed into the
 *  card's field. */
export type CardInput = CardAction | { buttonId: string; answer?: string; feedback?: string };

export function isButtonInput(i: CardInput): i is { buttonId: string; answer?: string; feedback?: string } {
  return typeof (i as { buttonId?: unknown }).buttonId === "string";
}

/** optionLabels reads an Interruption's options, which loomcycle carries as an
 *  untyped value: a list of strings, or of {label|value} objects. */
export function optionLabels(options: unknown): string[] {
  if (!Array.isArray(options)) return [];
  return options
    .map((o) => {
      if (typeof o === "string") return o;
      if (o && typeof o === "object") {
        const r = o as Record<string, unknown>;
        const v = r.value ?? r.label;
        return typeof v === "string" ? v : undefined;
      }
      return undefined;
    })
    .filter((o): o is string => !!o && o.length <= 200)
    .slice(0, 6);
}

/** isVerb reports whether v is one of the card verbs. */
export function isVerb(v: unknown): v is Verb {
  return typeof v === "string" && (Object.values(VERB) as string[]).includes(v);
}
