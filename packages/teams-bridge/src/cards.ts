// Adaptive Cards for the two things a loomcycle run can wait on a person for:
// a question (Interruption) and a held answer (review). Buttons use
// Action.Execute, so Teams sends an invoke the bridge answers with a
// replacement card, and a used card cannot be clicked twice.

export type Card = Record<string, unknown>;

export const VERB = {
  answer: "lc.answer",
  decline: "lc.decline",
  approve: "lc.approve",
  reject: "lc.reject",
} as const;

export type Verb = (typeof VERB)[keyof typeof VERB];

/** What a card button sends back. Inputs (answer, feedback) are merged in by Teams. */
export interface CardAction {
  verb: Verb;
  runId: string;
  interruptId?: string;
  answer?: string;
  feedback?: string;
}

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

/** parseCardAction reads an Action.Execute invoke's action. Returns undefined
 *  for anything this bridge did not issue. */
export function parseCardAction(value: unknown): CardAction | undefined {
  const action = (value as { action?: { verb?: unknown; data?: unknown } } | null)?.action;
  const verb = action?.verb;
  if (typeof verb !== "string" || !Object.values(VERB).includes(verb as Verb)) return undefined;
  const data = (action?.data ?? {}) as Record<string, unknown>;
  const str = (k: string) => (typeof data[k] === "string" ? (data[k] as string) : undefined);
  const runId = str("runId");
  if (!runId) return undefined;
  return {
    verb: verb as Verb,
    runId,
    interruptId: str("interruptId"),
    answer: str("answer")?.trim() || undefined,
    feedback: str("feedback")?.trim() || undefined,
  };
}
