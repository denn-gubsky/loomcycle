// Adaptive Cards for the two things a loomcycle run can wait on a person for:
// a question (Interruption) and a held answer (review). Buttons use
// Action.Execute, so Teams sends an invoke the bridge answers with a
// replacement card, and a used card cannot be clicked twice.
//
// A Teams button carries the whole action in its data (Teams merges the
// card's inputs into it), so the core's short button ids are not needed here.

import { isVerb, VERB, type CanonicalCard, type CardAction } from "@loomcycle/bridge-core";

export type Card = Record<string, unknown>;

function card(body: unknown[], actions: unknown[] = []): Card {
  return {
    type: "AdaptiveCard",
    $schema: "http://adaptivecards.io/schemas/adaptive-card.json",
    version: "1.5",
    body,
    ...(actions.length ? { actions } : {}),
  };
}

const text = (t: string, extra: Record<string, unknown> = {}) => ({ type: "TextBlock", text: t, wrap: true, ...extra });

export function questionCard(opts: {
  runId: string;
  interruptId: string;
  question: string;
  context?: string;
  options: string[];
}): Card {
  const base = { runId: opts.runId, interruptId: opts.interruptId };
  const body: unknown[] = [text(opts.question || "The agent has a question.", { weight: "Bolder" })];
  if (opts.context) body.push(text(opts.context, { isSubtle: true, size: "Small" }));
  const actions: unknown[] = [];
  if (opts.options.length > 0) {
    for (const o of opts.options) {
      actions.push({ type: "Action.Execute", title: o, verb: VERB.answer, data: { ...base, answer: o } });
    }
  } else {
    body.push({ type: "Input.Text", id: "answer", isMultiline: true, placeholder: "Your answer" });
    actions.push({ type: "Action.Execute", title: "Send", verb: VERB.answer, data: base });
  }
  actions.push({ type: "Action.Execute", title: "Decline", verb: VERB.decline, data: base });
  return card(body, actions);
}

export function reviewCard(opts: { runId: string; round?: number }): Card {
  const base = { runId: opts.runId };
  const title = opts.round && opts.round > 1 ? `The revised answer (round ${opts.round}) is ready for review.` : "The answer is ready for review.";
  return card(
    [
      text(title, { weight: "Bolder" }),
      { type: "Input.Text", id: "feedback", isMultiline: true, placeholder: "Feedback (to send it back for a revision)" },
    ],
    [
      { type: "Action.Execute", title: "Approve", verb: VERB.approve, data: base },
      { type: "Action.Execute", title: "Send back", verb: VERB.reject, data: base },
    ],
  );
}

/** The card that replaces a used one. */
export function resultCard(message: string): Card {
  return card([text(message, { isSubtle: true })]);
}

/** renderCard turns the core's card into the Adaptive Card Teams shows. */
export function renderCard(c: CanonicalCard): Card {
  switch (c.kind) {
    case "question":
      return questionCard({ runId: c.runId, interruptId: c.interruptId, question: c.question, context: c.context, options: c.options });
    case "review":
      return reviewCard({ runId: c.runId, round: c.round });
    case "result":
      return resultCard(c.message);
  }
}

/** parseCardAction reads an Action.Execute invoke's action. Returns undefined
 *  for anything this bridge did not issue. */
export function parseCardAction(value: unknown): CardAction | undefined {
  const action = (value as { action?: { verb?: unknown; data?: unknown } } | null)?.action;
  const verb = action?.verb;
  if (!isVerb(verb)) return undefined;
  const data = (action?.data ?? {}) as Record<string, unknown>;
  const str = (k: string) => (typeof data[k] === "string" ? (data[k] as string) : undefined);
  const runId = str("runId");
  if (!runId) return undefined;
  return {
    verb,
    runId,
    interruptId: str("interruptId"),
    answer: str("answer")?.trim() || undefined,
    feedback: str("feedback")?.trim() || undefined,
  };
}
