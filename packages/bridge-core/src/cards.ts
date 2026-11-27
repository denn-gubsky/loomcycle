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
      /** What the person reads besides the question: with options, every
       *  option numbered and how to answer (numberedOptions), so a renderer
       *  shows them all by showing the context; in a shared chat, who may
       *  answer. */
      context?: string;
      /** Up to MAX_OPTIONS choices. Empty: the question wants a free-text answer. */
      options: string[];
      /** One per option for the first MAX_OPTION_BUTTONS options, or one
       *  "send" button for a free-text answer, then decline. */
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

/** The most options a question card offers (RFC HG D4): each is listed
 *  numbered in the card's text and can be answered by its number. */
export const MAX_OPTIONS = 20;

/** The most options that also get a button; the rest are answered by number. */
export const MAX_OPTION_BUTTONS = 6;

/** The line under the numbered options that says how to answer them. */
export const ANSWER_HINT = "Reply with a number, or tap a button.";
export const ANSWER_HINT_OTHER = "Reply with a number or type your own answer, or tap a button.";

/** numberedOptions is the text a question card shows for its options: each
 *  one numbered, then how to answer. Empty for a free-text question. */
export function numberedOptions(options: readonly string[], allowOther: boolean): string {
  if (options.length === 0) return "";
  const lines = options.map((o, i) => `${i + 1}. ${o}`).join("\n");
  return `${lines}\n\n${allowOther ? ANSWER_HINT_OTHER : ANSWER_HINT}`;
}

/** The longest reply pickOption reads as a number (MAX_OPTIONS has two digits). */
const MAX_NUMBER_DIGITS = 3;

/** pickOption reads a typed reply as one of the options (RFC HG §4.2): a
 *  number from 1 to n, optionally followed by "." or ")", or an option's own
 *  text, ignoring case and surrounding whitespace. A number out of range is
 *  tried as text, so an option that is itself a number is still found. It
 *  returns the option as offered, or undefined. No regular expression: the
 *  reply is a person's text, and a scan is linear whatever it holds. */
export function pickOption(reply: string, options: readonly string[]): string | undefined {
  const text = reply.trim();
  let digits = text;
  const last = digits.at(-1);
  if (last === "." || last === ")") digits = digits.slice(0, -1).trimEnd();
  if (digits.length > 0 && digits.length <= MAX_NUMBER_DIGITS && [...digits].every((c) => c >= "0" && c <= "9")) {
    const n = Number(digits);
    if (n >= 1 && n <= options.length) return options[n - 1];
  }
  const wanted = text.toLowerCase();
  return options.find((o) => o.trim().toLowerCase() === wanted);
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
    .slice(0, MAX_OPTIONS);
}

/** isVerb reports whether v is one of the card verbs. */
export function isVerb(v: unknown): v is Verb {
  return typeof v === "string" && (Object.values(VERB) as string[]).includes(v);
}
