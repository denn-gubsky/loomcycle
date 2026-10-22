// compactionMethod — a short label (plus tooltip lines) for what a run does with
// its context as the window fills: the retention mode, and when it distils.
//
// It reads the run's RECORDED compaction and context blocks (the run's spec:
// per-run overrides already merged over the agent's definition when the run
// started — the values a resume comes back on), and fills what neither set
// with the runtime's defaults.
//
// What the loop actually does, which the label has to match (internal/loop
// distillAtBoundary, backstopAvailable, stateful.go):
//   - append: compaction summarises at compaction.autocompact_at_pct. An UNSET
//     compaction.enabled still compacts there — the runtime's safety net — and
//     only an explicit `enabled: false` turns that off, leaving the Compact
//     button (and an exhaustion report) as the only recourse.
//   - recap: recaps evicted reasoning at context.autorecap_at_pct; compaction is
//     the fallback at its own threshold unless explicitly disabled.
//   - stateful: no transcript to summarise — the least significant state
//     entries are evicted at compaction.autocompact_at_pct, unless disabled.
//   - auto: resolved when the run starts — recap on a local model or an
//     interactive run, stateful otherwise. Only the interactive half is
//     knowable here.
// Every automatic trigger needs the provider to report a context window.

// The runtime's defaults, applied at use time when a field is unset. A copy —
// the UI renders before asking the server — kept honest by a test against a
// fixture generated from the Go constants (internal/config).
export const CONTEXT_DEFAULTS = {
  compaction: {
    autocompact_at_pct: 80,
    keep_first: true,
    keep_last_n: 4,
    target_percentage: 10,
  },
  context: {
    autorecap_at_pct: 80,
    keep_last_n: 6,
    mode: "append",
    reasoning: "recap",
    recap_max_chars: 512,
  },
};

export interface CompactionBlock {
  enabled?: boolean;
  target_percentage?: number;
  keep_last_n?: number;
  keep_first?: boolean;
  autocompact_at_pct?: number;
  model?: string;
  memory_flush?: boolean;
}

export interface ContextBlock {
  mode?: string;
  keep_last_n?: number;
  reasoning?: string;
  model?: string;
  autorecap_at_pct?: number;
  harvest_to_memory?: boolean;
}

export interface ContextMethod {
  label: string;
  details: string[];
}

const WINDOW_NOTE = "Automatic triggers fire only when the provider reports a context window.";

export function contextMethod(
  spec: { compaction?: CompactionBlock | null; context?: ContextBlock | null },
  opts: { interactive?: boolean } = {},
): ContextMethod {
  const c = spec.compaction ?? {};
  const x = spec.context ?? {};
  const D = CONTEXT_DEFAULTS;
  const compactAt = c.autocompact_at_pct ?? D.compaction.autocompact_at_pct;
  const compactionOff = c.enabled === false;
  let mode = x.mode || D.context.mode;
  let prefix = "";
  const details: string[] = [];

  if (mode === "auto") {
    if (!opts.interactive) {
      return {
        label: "auto",
        details: [
          "context.mode auto: chosen when the run starts — recap on a local model or an interactive run, stateful otherwise.",
          WINDOW_NOTE,
        ],
      };
    }
    details.push("context.mode auto resolves to recap for an interactive run.");
    prefix = "auto → ";
    mode = "recap";
  }

  if (mode === "stateful") {
    details.push("Structured state replaces the history; there is no transcript to summarise.");
    if (compactionOff) {
      details.push("State eviction is off (compaction.enabled: false).");
      return { label: prefix + "stateful", details };
    }
    details.push(`When the prompt reaches ${compactAt}% of the window, the least significant state entries are evicted.`);
    details.push(WINDOW_NOTE);
    return { label: `${prefix}stateful · evicts at ${compactAt}%`, details };
  }

  if (mode === "recap") {
    const recapAt = x.autorecap_at_pct ?? D.context.autorecap_at_pct;
    const reasoning = x.reasoning || D.context.reasoning;
    details.push(
      `Recaps when the prompt reaches ${recapAt}% of the window: evicted reasoning — ${reasoning}; keeps the last ${
        x.keep_last_n ?? D.context.keep_last_n
      } messages verbatim.`,
    );
    if (x.model) details.push(`Recap model: ${x.model}.`);
    if (x.harvest_to_memory) details.push("Evicted spans are banked for long-term memory.");
    details.push(
      compactionOff
        ? "No compaction fallback (compaction.enabled: false)."
        : `Compaction is the fallback when a recap does not reclaim the window, at ${compactAt}%:`,
    );
    if (!compactionOff) details.push(...summaryDetails(c));
    details.push(WINDOW_NOTE);
    return { label: `${prefix}recap · auto at ${recapAt}%`, details };
  }

  // append (and anything unrecognised, which the server would have refused).
  if (compactionOff) {
    return {
      label: "append · manual compact",
      details: [
        "Full history is sent every turn. Auto-compaction is off (compaction.enabled: false): use Compact to summarise; the run reports when its window is not being reclaimed.",
        ...summaryDetails(c),
      ],
    };
  }
  details.push(
    c.enabled
      ? `Full history is sent every turn, and compacted when the prompt reaches ${compactAt}% of the window.`
      : `Full history is sent every turn. When the prompt reaches ${compactAt}% of the window it is compacted anyway — the runtime's safety net, on unless compaction.enabled is false.`,
  );
  details.push(...summaryDetails(c), WINDOW_NOTE);
  return { label: `append · auto-compact at ${compactAt}%`, details };
}

// summaryDetails describes what one compaction keeps and how it summarises.
function summaryDetails(c: CompactionBlock): string[] {
  const D = CONTEXT_DEFAULTS.compaction;
  const out = [
    `A compaction keeps the last ${c.keep_last_n ?? D.keep_last_n} messages verbatim${
      c.keep_first ?? D.keep_first ? " and the first (the task)" : ""
    }; the summary aims for ~${c.target_percentage ?? D.target_percentage}% of what it replaces.`,
  ];
  if (c.model) out.push(`Summary model: ${c.model}.`);
  if (c.memory_flush) out.push("Compacted spans are banked for long-term memory (memory flush on).");
  return out;
}

/** The method of a run from its recorded spec (Agent.spec), or null when the
 *  spec cannot say: absent, or a no-trace run's record, which holds its
 *  retention and nothing else. Fields that are not the expected shape are
 *  ignored rather than trusted. */
export function contextMethodFromSpec(spec: unknown, opts: { interactive?: boolean } = {}): ContextMethod | null {
  if (!spec || typeof spec !== "object") return null;
  const rec = spec as Record<string, unknown>;
  if (rec.retention === "none") return null;
  const obj = (v: unknown) => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined);
  return contextMethod(
    { compaction: obj(rec.compaction) as CompactionBlock | undefined, context: obj(rec.context) as ContextBlock | undefined },
    opts,
  );
}
