/**
 * Wire-shape types for the loomcycle HTTP+SSE surface. Field names use
 * snake_case to match the Go server's JSON output (no client-side
 * conversion — what's on the wire is what you get).
 *
 * Public API method *parameters* use camelCase (JS norm); see
 * `client.ts` for the input shapes (RunOptions, CreateSnapshotOptions,
 * etc.) — those are translated to snake_case in the request body.
 */

// ---- Run lifecycle ----

export type EventType =
  | "started"
  | "text"
  | "tool_call"
  | "tool_result"
  | "usage"
  | "done"
  | "error"
  // v0.8.x extras already on the wire from internal/providers/provider.go.
  // Adding them here as additive options — consumers that only switch on
  // the base seven keep working, while consumers that already render
  // retries/host-widening/etc. can type-narrow.
  | "retry"
  | "host_widened"
  // v0.4 side-channel SSE frames emitted via sse.sendRaw (the JSON payload
  // doesn't carry `type` server-side; parseSSE backfills it from the SSE
  // event-name so the consumer sees a well-formed AgentEvent).
  | "session"
  | "agent"
  // RFC AI interactive session frames. `awaiting_input` = a persistent run
  // parked at end_turn (idle, ready for steering). `steer` = an operator
  // turn drained into the conversation, OR (on a streamRunByID re-attach) a
  // replayed prior operator turn (user_input.source === "replay").
  // `context_compaction` marks a context summarization.
  | "awaiting_input"
  | "steer"
  | "context_compaction"
  // RFC AW per-scope token budgets. `limit` = a server-generated token-budget
  // crossing (a soft warning at run start, or a soft crossing mid-run). The
  // structured payload rides `AgentEvent.limit`.
  | "capability_inert"
  | "limit"
  // RFC DC per-run overrides. `override` = a server-generated notice that the
  // RUN's own configuration changed while it was running, because an operator
  // retuned it. Distinct from a provider fallback on purpose: a fallback means
  // the runtime moved the run because something FAILED, this means a person
  // chose to, and a UI should not render a deliberate model change as an
  // outage. The structured payload rides `AgentEvent.override`.
  | "override"
  // ⚠️ THESE SIXTEEN WERE ON THE WIRE BEFORE THEY WERE IN THIS UNION, some of
  // them for a dozen releases. A consumer narrowing on `ev.type` could not
  // NAME them, so the only way to render one was to cast out of the union —
  // and a value a typed client cannot name is a value it is likely to drop.
  //
  // It cost an incident: v1.85.0 began emitting `context_exhausted`, a type
  // that had never existed on the wire, into a terminal that had no case for
  // it. A server change is only half of a two-repo feature; this file is the
  // other half, and it is now swept against internal/providers/provider.go
  // rather than extended one value at a time.
  //
  // Context visibility — what the distillation subsystem did, declined to do,
  // or could not do. `context_recap` (L1 recap ran), `context_state` (the
  // stateful Σ after a step, incl. any evicted keys), `context_distill_declined`
  // (a tier ran and refused, with the reason), `context_exhausted` (nothing
  // reclaimed the window and the run is heading for the provider's limit).
  | "context_recap"
  | "context_state"
  | "context_distill_declined"
  | "context_exhausted"
  // The model's streamed reasoning trace, forwarded live. Not accumulated into
  // content and not echoed into the next request — the full trace rides
  // `done`.
  | "thinking"
  // RFC BH: the operator stopped THIS turn (the run itself continues, and an
  // interactive run parks).
  | "turn_cancelled"
  // Routing: the runtime moved the run because something FAILED — a provider
  // swap, a swap it refused to make, a model downgrade, and the two
  // cache/reasoning invalidations a swap forces.
  | "provider_fallback"
  | "fallback_suppressed"
  | "model_downgraded"
  | "cache_invalidated"
  | "reasoning_invalidated"
  // Channels + interruptions raised from inside a run.
  | "channel_publish"
  | "channel_delivery"
  | "interruption_pending"
  // Agent fan-out ledger (a parent's parallel_spawn), which is what makes an
  // in-flight child durable across a pause.
  | "spawn_child_started"
  | "spawn_child_result"
  // The prompt a run's first model call received (RFC DI). A transcript
  // record read via getRunPrompt; never sent on the live stream.
  | "prompt_snapshot"
  // v0.9.x — client-synthesized lifecycle events emitted ONLY when the
  // streaming caller passes `debug: true`. Never originate from the
  // server. The leading underscore signals "synthetic, not on the wire."
  // Consumers that opt out (default) never see this `type`.
  | "_meta";

export interface ToolUse {
  id: string;
  name: string;
  input: unknown;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
  model?: string;
  /** Serving model's context-window ceiling, stamped by the loop on each
   *  per-iteration usage event from Provider.Capabilities(). 0/absent =
   *  unknown (e.g. Ollama). Lets a client render a "context used / max"
   *  gauge without a hard-coded per-model table. Additive + optional. */
  max_context_tokens?: number;
  /** Forward-compat: a future wire bump may include the provider-billed
   *  USD cost alongside token counts. Today the sidecar never populates
   *  this; the field stays optional so consumers can plumb it without a
   *  wire change. */
  cost_usd?: number;
}

/** RetryInfo accompanies an `event: retry` frame (EventRetry in the Go
 *  server). Surfaced live during the retry sleep — useful for "waiting on
 *  rate limit" UI. The agent loop is unaffected; the retry is invisible
 *  to it. Wire-stable; mirrors providers.RetryInfo. */
export interface RetryInfo {
  provider: string;
  attempt: number;
  wait_ms: number;
  /** One of providers.RetryReason* constants — "retry-after header" or
   *  "exponential backoff" today. Stable wire string; do not parse. */
  reason: string;
}

/** HostWidening accompanies an `event: host_widened` frame (v0.8.17+).
 *  Emitted once per dispatched tool call whose Pre-hook allow_hosts grant
 *  fired. Operators audit confused-deputy patterns by comparing `url`'s
 *  host to `hosts_added`. Wire-stable; mirrors providers.HostWideningEventInfo. */
export interface HostWidening {
  tool_call_id: string;
  tool_name: string;
  url: string;
  hook_owner: string;
  hook_name: string;
  hosts_added: string[];
}

/** LimitInfo accompanies an `event: limit` frame (RFC AW per-scope token
 *  budgets). Names which scope tripped, how hard (`soft` warns + the run
 *  continues; `hard` means the NEXT run is refused at admission), and where the
 *  scope stands against its ceiling — so a UI can render "tenant acme at 1.2M /
 *  1M tokens this month" without a follow-up fetch. Wire-stable; mirrors
 *  providers.LimitInfo. */
/** A run's own stored overrides — what the RUN set, not what it will
 *  effectively use. Mirrors the `config` object on `GET /v1/runs/{id}/config`
 *  and on the `retuneRun` reply.
 *
 *  A field absent here means "this run does not override it", which is a
 *  different and more useful answer at this layer than a resolved value would
 *  be: it says the definition, the tier or a driver still decides. Use
 *  {@link LoomcycleClient.getEffectiveConfig} for the resolved view. */
export interface RunConfigRecord {
  sampling?: Record<string, unknown>;
  compaction?: Record<string, unknown>;
  context?: Record<string, unknown>;
  max_context_tokens?: number;
  run_timeout_seconds?: number;
  routing?: {
    provider?: string;
    model?: string;
    tier?: string;
    effort?: string;
  };
  resources?: {
    max_tokens?: number;
    max_iterations?: number;
    unbounded_iterations?: boolean;
    max_concurrent_children?: number;
  };
  tuning?: {
    retry_attempts?: number;
    memory_inject_max_tokens?: number;
    memory_index_max_bytes?: number;
    inject_tool_guide?: boolean;
  };
  interactive?: boolean;
  interruption?: { enabled?: boolean; kinds?: string[]; max_pending?: number };
  hosts?: Record<string, unknown>;
}

/** Which layer decided an effective value.
 *
 *  This is the half that makes the report worth fetching. `max_iterations: 16`
 *  cannot distinguish a deliberate setting from a default nobody chose, and
 *  those call for opposite actions — so every field carries where it came from.
 *
 *  - `run`        — a per-run override set it
 *  - `definition` — the agent definition set it
 *  - `user_tier`  — operator tier policy (e.g. retry_attempts)
 *  - `operator`   — operator env / global configuration
 *  - `resolved`   — decided at runtime: the tier cascade, the driver, the model.
 *                   A `resolved` field with a null value means the runtime
 *                   settles it somewhere this report cannot see, which is a
 *                   more honest answer than omitting the field.
 *  - `default`    — a fixed constant in the runtime */
export type EffectiveConfigSource =
  | "run"
  | "definition"
  | "user_tier"
  | "operator"
  | "resolved"
  | "default";

/** One field's effective value plus the layer that decided it. */
export interface EffectiveValue {
  value: unknown;
  source: EffectiveConfigSource;
}

/** The reply from `GET /v1/runs/{run_id}/config` — what the RUN overrides. */
export interface RunConfigResponse {
  run_id: string;
  agent: string;
  /** The model the run last resolved to, as recorded on the run row. */
  model: string;
  config: RunConfigRecord;
}

/** Payload on a `prompt_snapshot` transcript event (RFC DI): the system blocks
 *  and the run's input its first model call received. */
export interface RunPromptSnapshot {
  system: PromptBlock[];
  input: PromptBlock[];
}

/** One block of a {@link RunPromptResponse}. `text` blocks carry the prompt
 *  text; an `image` block keeps its `media_type` but not its bytes. */
export interface PromptBlock {
  type: string;
  text?: string;
  media_type?: string;
}

/** The reply from `GET /v1/runs/{run_id}/prompt` (RFC DI) — what the run's first
 *  model call was sent, as assembled: skills, memory injection, `{{...}}`
 *  expansion and metadata already applied. `input` is this run's own input
 *  (for a continuation, the new message rather than the whole history). */
export interface RunPromptResponse {
  run_id: string;
  system: PromptBlock[];
  input: PromptBlock[];
}

/** The reply from `GET /v1/runs/{run_id}/effective-config` — for every
 *  overridable field, the value this run will actually use and which layer
 *  decided it.
 *
 *  Keyed by the wire name the rest of the API uses (`max_tokens`, not
 *  `maxTokens`), so it joins directly against a definition from
 *  `/v1/_library/agents`. */
export interface EffectiveConfigResponse {
  run_id: string;
  agent: string;
  fields: Record<string, EffectiveValue>;
  /** Settings this run carries that CANNOT take effect — the advisory half of
   *  the report, and the half a `fields` map structurally cannot express.
   *
   *  `fields` answers "what value is in force"; a setting can be in force and
   *  still be inert, because a DIFFERENT setting disables it — a compaction
   *  threshold under a mode that never consults compaction, a `memory_flush`
   *  whose banking is gated on `harvest_to_memory`. An operator reading only
   *  `fields` sees their value and concludes it is doing something.
   *
   *  Always present, `[]` when nothing is inert, so a consumer can tell "this
   *  run has no traps" from "this server does not report them". */
  inert: InertContextSetting[];
}

/** One entry in {@link EffectiveConfigResponse.inert}. */
export interface InertContextSetting {
  /** The yaml path that cannot take effect, e.g. `compaction.memory_flush`. */
  setting: string;
  /** Why — named in terms of the OTHER setting that disables it, because that
   *  is the one the operator has to change their mind about. */
  reason: string;
  /** The setting that DOES do what they were reaching for. Absent when there
   *  is no equivalent, which is itself the answer; an advisory without it
   *  leaves the operator hunting for the live knob. */
  fix?: string;
}

/** The reply from `retuneRun` — the run's MERGED configuration, not an echo of
 *  the request.
 *
 *  A caller cannot recompute it: the merge is not a field-wise union. Naming a
 *  `model` clears the `provider` so a previous choice cannot contradict the new
 *  pin, and naming a `tier` clears the `model`. */
export interface RetuneRunResponse {
  run_id: string;
  retuned: boolean;
  config: RunConfigRecord;
}

/** OverrideInfo accompanies an `event: override` frame (RFC DC per-run
 *  overrides): a run's own configuration changed mid-run because an operator
 *  retuned it.
 *
 *  TWO EVENTS CARRY THIS TYPE, and a consumer needs to tell them apart because
 *  one retune can produce both. The server emits one when the operator acts,
 *  listing in `fields` the keys the REQUEST set and carrying NO from/to pair —
 *  nothing has been re-resolved yet, so there is no honest "to" to report. The
 *  runtime emits one when the run ADOPTS a routing change, and that one always
 *  carries both halves of the pair.
 *
 *  So: a pair present means "the run is now using this"; a pair absent means
 *  "an operator asked for these fields". Filter on `from_model === undefined`
 *  for the second kind.
 *
 *  `fields` used to be documented as the request's keys unconditionally while
 *  the only site filling it in was the runtime's, which cannot see a request —
 *  so a branch written for "max_tokens changed" could never run.
 *
 *  Carries only operator-chosen configuration whose effects are already visible
 *  — a model name, a budget. No credential, no operator host. */
export interface OverrideInfo {
  /** Who changed it. "operator" today; present so a later automatic retune is
   *  distinguishable rather than indistinguishable. */
  source: string;
  /** "provider/model" before the change. Absent when routing did not move. */
  from_model?: string;
  /** "provider/model" after the change. */
  to_model?: string;
  /** The override keys the request actually set — so a budget or tuning change
   *  that moved no model is still legible. */
  fields?: string[];
}

/** The machine-readable half of a terminal run failure, carried on
 *  `event: error` frames.
 *
 *  Once the SSE stream is open the HTTP status is already 200, so without this
 *  a consumer's only signal is the event type plus an English string —
 *  "retry shortly" and "your budget is exhausted" look identical and want
 *  opposite responses. */
export interface ErrorInfo {
  /** "transient" | "validation" | "business" | "permission". */
  category: string;
  /** Answers only "will resending this exact call fail?" — not "should I give
   *  up". A false here still leaves the alternatives in `description` open. */
  is_retryable: boolean;
  /** What went wrong AND what to do next. */
  description?: string;
  /** Backoff hint, meaningful only when `is_retryable`. ABSENT rather than 0
   *  when there is no hint: an absent hint and a zero one are opposite
   *  instructions. */
  retry_after_ms?: number;
}

/** The payload on a `capability_inert` event: one tool the agent holds and
 *  cannot use, because the capability gate that tool reads grants nothing.
 *
 *  Server-generated, emitted ONCE at run start — the condition is a property of
 *  the definition, not of any one call. It carries the FIX as well as the fact:
 *  the governing yaml key is not guessable from the tool name, which is most of
 *  why this failure was hard to act on. */
export interface CapabilityInertInfo {
  /** The granted tool, as named in the agent's `tools` list. */
  tool: string;
  /** The yaml key that governs it, e.g. `agent_def_scopes`. */
  gate: string;
  /** A line naming the tool, the gate, and what to set. */
  message?: string;
}

export interface LimitInfo {
  /** Which axis tripped: "operator" | "tenant" | "user". */
  scope: string;
  /** The tripped scope's id — tenant id (scope=tenant), user subject
   *  (scope=user), "" (operator-global). */
  scope_id?: string;
  /** "soft" (warn, run continues) | "hard" (next run refused at admission). */
  severity: string;
  /** Budget window; "month" (calendar month, UTC) in Phase 1. */
  window: string;
  /** The scope's month-to-date token total at the crossing. */
  used: number;
  /** The tier that was crossed (the soft or hard ceiling). */
  limit: number;
  /** Human-readable banner string. Optional. */
  message?: string;
}

/** A tier's verdict inside {@link ContextExhaustedInfo} — what was tried and
 *  why it refused. Carried verbatim from each decline rather than summarised,
 *  because each already names its own fix. */
export interface ContextTierVerdict {
  /** "recap" | "compaction" | "stateful". */
  mode: string;
  /** Same vocabulary as {@link ContextDistillDeclinedInfo.reason}. */
  reason?: string;
  message?: string;
}

/** Payload on `context_distill_declined` — a distillation tier ran and refused.
 *
 *  The coarse branch is the event's existence: a run that distilled emits
 *  `context_recap` / `context_compaction` instead. `reason` says which refusal,
 *  and new values are ADDITIVE — a consumer that does not know one should fall
 *  back to `message`, never drop the frame. */
export interface ContextDistillDeclinedInfo {
  /** Which tier declined: "recap" | "compaction". */
  mode: string;
  /** What opened the gate: "auto" | "self" | "manual". */
  trigger?: string;
  /** "split_declined" | "empty_summary" | "summary_error" | "not_smaller" |
   *  "reasoning_keep" | "noop" | "noop_keep_spans_all" | "noop_not_smaller". */
  reason: string;
  used_tokens?: number;
  window_tokens?: number;
  /** Conversation length at the refusal. */
  messages?: number;
  keep_last_n?: number;
  before_tokens?: number;
  after_tokens?: number;
  /** "info" | "warning". `warning` means this path cannot reclaim the window
   *  and will decline identically again; `info` means the mechanism is healthy
   *  and the refusal was correct for this input. */
  severity?: string;
  message?: string;
}

/** Payload on `context_exhausted` — every tier ran (or could not) and the
 *  window is still full. Reported at most once per run per 10-point band, and
 *  ONLY from a footprint a provider actually returned: before the first turn
 *  the numbers are an estimate of a request that has not been sent, and the run
 *  says nothing. */
export interface ContextExhaustedInfo {
  used_tokens: number;
  window_tokens: number;
  used_pct?: number;
  /** Every tier's explanation, not just the last — a run can decline twice for
   *  DIFFERENT reasons, and half the fix is not a fix. */
  verdicts?: ContextTierVerdict[];
  message?: string;
}

/** Payload on `context_compaction` — the L0 summarize-and-keep-tail form. */
export interface ContextCompactionInfo {
  summary: string;
  before_tokens?: number;
  after_tokens?: number;
  keep_n?: number;
  keep_first?: boolean;
  trigger?: string;
  /** Set when the evicted span was banked to persistent memory. */
  memory_banked?: { pending_id?: string; messages?: number; error?: string };
}

/** Payload on `context_recap` — the L1 form: the evicted span becomes a running
 *  note and the last N turns stay verbatim. */
export interface ContextRecapInfo {
  recap: string;
  before_tokens?: number;
  after_tokens?: number;
  keep_n?: number;
  keep_first?: boolean;
  trigger?: string;
  /** "recap" | "drop" — what happened to the evicted reasoning. */
  reasoning?: string;
}

/** Payload on `context_state` — the L2 stateful form, emitted once per step. */
export interface ContextStateInfo {
  /** Σ after the step's patch was merged. */
  state: Record<string, unknown>;
  /** What this step changed. */
  patch?: Record<string, unknown>;
  iter: number;
  action?: string;
  reasoning?: string;
  /** A schema the model proposed. INERT — recorded for an operator to adopt by
   *  forking the agent def, never applied to this run's validation. */
  proposed_schema?: Record<string, unknown>;
  /** Σ keys dropped by retention-class eviction this step. Banked to memory
   *  before they went, so a later recall can fetch them back. */
  evicted?: string[];
}

/** Payload on `provider_fallback` / `fallback_suppressed` / `model_downgraded` —
 *  the runtime moved (or refused to move) the run after a failure. */
export interface FallbackInfo {
  failed_provider: string;
  failed_model: string;
  new_provider?: string;
  new_model?: string;
  attempt: number;
  user_tier: string;
  reason: string;
  cause_error?: string;
}

/** Payload on `channel_publish` / `channel_delivery`. */
export interface ChannelEventInfo {
  channel: string;
  message_id: string;
  scope: string;
  scope_id?: string;
  payload_bytes: number;
  payload_preview?: string;
  dropped_oldest?: number;
  cursor?: string;
}

/** Payload on `interruption_pending` — the run asked a human a question and is
 *  waiting on the answer. */
export interface InterruptionEventInfo {
  interrupt_id: string;
  kind: string;
  question?: string;
  options?: unknown;
  context?: string;
  priority: string;
  expires_at?: string;
}

/** Payload on `turn_cancelled` (RFC BH). */
export interface TurnCancelledInfo {
  reason?: string;
  since_turn: number;
}

/** Payload on `spawn_child_started` / `spawn_child_result` — the two-event
 *  ledger a fan-out parent writes so an in-flight child survives a pause. */
export interface SpawnChildInfo {
  tool_use_id: string;
  index: number;
  run_id?: string;
  agent?: string;
  ok?: boolean;
  output?: string;
  error?: string;
  state?: Record<string, unknown>;
}

export interface AgentEvent {
  type: EventType;
  text?: string;
  tool_use?: ToolUse;
  usage?: Usage;
  error?: string;
  /** is_error flags a tool_result whose execution failed. Surviving the
   *  persist+replay round-trip matters because a continuation that lost
   *  the flag would re-feed the model a successful-looking result. */
  is_error?: boolean;
  stop_reason?: string;
  /** Retry payload on `event: retry`. Nil on all other event types. */
  retry?: RetryInfo;
  /** Host-widening payload on `event: host_widened`. Nil on all other
   *  event types. */
  host_widening?: HostWidening;
  /** Payload on `event: awaiting_input` (RFC AI) — a persistent interactive
   *  run parked at end_turn. `since_turn` is the iteration it parked after. */
  awaiting_input?: { since_turn?: number };
  /** Payload on `event: steer` (RFC AI) — the operator's drained turn. On a
   *  re-attach replay, `source` is `"replay"`. Nil on all other event types. */
  user_input?: { text?: string; source?: string; seen_at?: string };
  /** Payload on `event: limit` (RFC AW) — a per-scope token-budget crossing.
   *  Nil on all other event types. */
  limit?: LimitInfo;
  /** Set on `capability_inert` events: a tool the agent holds and cannot use. */
  capability_inert?: CapabilityInertInfo;
  /** Set on `event: override` frames — what the operator changed, and from
   *  what to what. */
  override?: OverrideInfo;
  /** Payload on `event: error` — the classification of a TERMINAL run failure.
   *  Absent on every other event type, and absent for a failure the runtime
   *  cannot categorise: there is deliberately no "unknown" category, so a
   *  missing `error_info` means "not classified", never "classified as
   *  nothing in particular". */
  error_info?: ErrorInfo;
  // v0.4 `event: agent` side-channel announces the run's tracking IDs
  // immediately after the `event: session` frame. parent_agent_id is null
  // for top-level runs.
  agent_id?: string;
  run_id?: string;
  session_id?: string;
  parent_agent_id?: string | null;
  // v0.12.x — opaque caller-tracking lineage on the `event: agent` frame
  // (and inherited by sub-agents). Present only when the run carried it.
  parent_context?: ParentContext;
  /** Payload on `context_compaction`. */
  context_compaction?: ContextCompactionInfo;
  /** Payload on `context_recap`. */
  context_recap?: ContextRecapInfo;
  /** Payload on `context_state`. */
  context_state?: ContextStateInfo;
  /** Payload on `context_distill_declined`. Named `context_distill` on the
   *  wire, not `context_distill_declined` — the field is the server struct's
   *  json tag and does not mirror the event name. */
  context_distill?: ContextDistillDeclinedInfo;
  /** Payload on `context_exhausted`. */
  context_exhausted?: ContextExhaustedInfo;
  /** Payload on `provider_fallback` / `fallback_suppressed` /
   *  `model_downgraded`. */
  fallback?: FallbackInfo;
  /** Payload on `channel_publish` / `channel_delivery`. */
  channel?: ChannelEventInfo;
  /** Payload on `interruption_pending`. */
  interruption?: InterruptionEventInfo;
  /** Payload on `turn_cancelled`. */
  turn_cancelled?: TurnCancelledInfo;
  /** Payload on `spawn_child_started` / `spawn_child_result`. */
  spawn_child?: SpawnChildInfo;
  /** Payload on `prompt_snapshot` (transcript only). */
  prompt_snapshot?: RunPromptSnapshot;
  /** The assistant turn's accumulated reasoning trace, on `done`. Empty for
   *  non-thinking models. */
  reasoning?: string;
  // v0.9.x — client-synthesized observability fields, populated only on
  // events of `type: "_meta"`. `meta_subtype` is always set on a
  // _meta event (distinguishes open from close). `meta_reason` is set
  // ONLY on stream_close frames — carrying the cause ("eof" on clean
  // close, "AbortError"/"AuthError"/etc on a typed-error throw). The
  // stream_open frame leaves meta_reason undefined — the frame itself
  // is the signal that the stream just opened.
  meta_subtype?: "stream_open" | "stream_close";
  meta_reason?: string;
}

export type PromptContent =
  | { type: "trusted-text"; text: string; cacheable?: boolean }
  | { type: "untrusted-block"; kind: string; text: string }
  // Image input (RFC AT), valid only in a user segment. `data` is the
  // base64-encoded image bytes with NO "data:" prefix; there is deliberately
  // no URL form (SSRF). The model must be vision-capable or the run errors
  // before the call.
  | { type: "image"; media_type: ImageMediaType; data: string };

/** Whitelisted image media types accepted on an `image` content block (RFC AT). */
export type ImageMediaType =
  | "image/png"
  | "image/jpeg"
  | "image/gif"
  | "image/webp";

export interface PromptSegment {
  role: "system" | "user";
  content: PromptContent[];
}

/** The per-run overrides (RFC DC) — a run's own answer to how it should run,
 *  instead of its agent definition's. Persisted with the run, so they survive a
 *  pause. A parked run is retuned with `retuneRun()` — or with
 *  `sendRunInput(id, text, { overrides })` when you also want to say something.
 *
 *  This line used to say "can also be retuned via `sendRunInput`", which was
 *  true of the WIRE and not of the method: the adapter sent a bare `{text}`, so
 *  the sentence described a capability no caller could reach through it. A
 *  comment that names the transport instead of the API is how a gap reads as
 *  closed.
 *
 *  Declared ONCE and extended by both {@link RunOptions} and
 *  {@link ContinueOptions}, because the serializer that writes them is shared
 *  by both paths — a copy on one interface and not the other compiles as a type
 *  error at best and drops the caller's value at worst.
 */
export interface RunOverrideOptions {

  /** Run on a specific model. Must be one the agent's definition already
   *  allows — an override selects WITHIN that set and cannot widen it, so
   *  which vendor sees the conversation stays an operator decision. Naming a
   *  model PINS it: the tier stops choosing and stops falling back. */
  model?: string;
  /** Run on a specific provider. Must be one the definition already allows.
   *  Unlike `model` this NARROWS rather than pins — the tier still chooses the
   *  model within that vendor and still falls back within it. */
  provider?: string;
  /** Route through a different configured tier. */
  tier?: string;
  /** Reasoning-effort hint. Any value outside low|medium|high is REFUSED
   *  rather than ignored: "effort was dropped" and "effort was applied" look
   *  identical from the outside. */
  effort?: "low" | "medium" | "high";

  /** Per-reply output cap. May be RAISED above the agent's own. */
  maxTokens?: number;
  /** Loop bound. May be RAISED above the agent's own. */
  maxIterations?: number;
  /** Lift or restore the loop bound. `false` bounds an otherwise-unbounded
   *  agent for this run only — which is why it is a boolean you can set rather
   *  than a flag you can only turn on. */
  unboundedIterations?: boolean;
  /** How wide this run may fan out into sub-agents. May only be LOWERED below
   *  what the definition allows; raising it is refused, because a child takes
   *  no admission slot and is not budget-checked at spawn, so this is the only
   *  bound on fan-out that exists. */
  maxConcurrentChildren?: number;

  /** How many times to retry the same provider before falling back. 0 disables
   *  retrying for this run. */
  retryAttempts?: number;
  /** Token budget for memory injected into the prompt. 0 injects none. */
  memoryInjectMaxTokens?: number;
  /** Byte budget for the memory index. 0 omits it. */
  memoryIndexMaxBytes?: number;
  /** Whether to inject the generated tool guide into the prompt. */
  injectToolGuide?: boolean;
  /** Park this run at its turn boundaries instead of finishing, so an operator
   *  can steer it — settable while the run is ALREADY GOING, which is the point:
   *  nobody knows at start that they will need to correct it.
   *
   *  `false` releases a run that was started interactive. Omit to keep whatever
   *  the run has; a retune of anything else must not disturb this. */
  interactive?: boolean;

  /** Let this run's agent ASK a human a question, overriding what its
   *  definition allows.
   *
   *  Overridable because an interruption touches no data and no host — it blocks
   *  and waits for a person — so the exposure is liveness, bounded by the run's
   *  timeout and the interruption's own. */
  interruption?: { enabled?: boolean; kinds?: string[]; max_pending?: number };

}

export interface RunOptions extends RunOverrideOptions {
  agent: string;
  segments: PromptSegment[];
  tools?: string[];
  /** Per-request URL allowlist (v0.3.3+). Three-state on the wire:
   *  - omitted / `undefined` — no narrowing (operator's static list applies).
   *  - `null` — same as omitted (pass-through; convenience for callers
   *    that thread a possibly-unset slice).
   *  - `[]` — deny all (every network call refuses).
   *  - `["host1.com", ...]` — intersection with the operator's list
   *    (caller can shrink, never widen). */
  allowedHosts?: string[] | null;
  /** Brave-side filtering when `allowedHosts` is set:
   *  - "drop" (default) — Brave results outside the intersected list
   *    are omitted; the model only sees URLs it can follow up with WebFetch.
   *  - "keep" — Brave's full result set passes through; caller filters
   *    downstream.
   *  Ignored when `allowedHosts` is unset. */
  webSearchFilter?: "drop" | "keep";
  /** Bind the run to an existing session (v0.x). When set, the new run is
   *  appended to that session (transcript is NOT replayed by /v1/runs —
   *  use continueSession for replay semantics). When empty, a fresh
   *  session is created and announced as the first SSE frame. */
  sessionId?: string;
  tenantId?: string;
  /** Caller-supplied user binding (v0.4+). Records the run under this
   *  user_id for cancel/list endpoints; sub-agents inherit it. Charset:
   *  [A-Za-z0-9_-]{1,128}. */
  userId?: string;
  /** Caller-supplied tracking handle (v0.4+). When omitted, the server
   *  generates one and announces it in `event: agent`. Addresses the run
   *  for status/cancel via /v1/agents/{agent_id}. Charset:
   *  [A-Za-z0-9_-]{1,128}. */
  agentId?: string;
  /** Per-user tier name (v0.8.2+). Maps to `user_tiers.{name}` in the
   *  sidecar config (provider_priority + optional per-agent overlay).
   *  Server 400s on unknown tier. When omitted, falls through to
   *  `user_tiers.default`. */
  userTier?: string;
  /** Per-run MCP bearer token (v0.8.x+). Substituted into MCP HTTP header
   *  values containing `${run.user_bearer}` at outbound request-build time.
   *  Charset: [A-Za-z0-9._\-+/=]{16,512}. Empty is backwards-compatible
   *  (static-bearer setups unaffected). Sub-agents inherit identically.
   *  Never persisted; never logged in full. */
  userBearer?: string;
  /** Per-tool named credentials map (v1.x RFC F). Per-MCP-server bearers
   *  keyed by operator-chosen name (convention: the `mcp_servers.<name>`
   *  yaml key). Substituted into MCP HTTP header values containing
   *  `${run.credentials.<name>}` at outbound request-build time. Keys
   *  match `[a-zA-Z0-9_-]{1,64}`; values arbitrary strings. Sub-agents
   *  inherit the whole map. Coexists with `userBearer` — the legacy
   *  field auto-promotes to `userCredentials.default` for back-compat
   *  with v0.8.x flows. Never persisted; never logged. */
  userCredentials?: Record<string, string>;
  /** Opaque caller-tracking lineage (v0.12.x). Carried verbatim by the
   *  runtime, inherited UNCHANGED by every sub-agent the Agent tool
   *  spawns, persisted on each run row, and echoed back on the per-agent
   *  report surfaces (agent status, run-state stream, the `event: agent`
   *  frame) — so you can attribute a child sub-agent's usage to the
   *  user-initiated request that spawned the whole tree. Not a secret.
   *  Omitted = no tracking context. */
  parentContext?: ParentContext;
  /** Optional NON-SECRET structured metadata passed to the agent (repo
   *  name, review policy, preferred skills, …) — symmetric with the
   *  WebHook/Schedule trigger paths. As a first-party (bearer-authed)
   *  caller this is TRUSTED: a code-js agent reads it as `input.metadata`;
   *  an LLM agent receives it as a trusted prompt block. NOT for secrets —
   *  use {@link RunOptions.userCredentials} for tokens. Per-call, not session
   *  state: a continuation does not inherit it — re-send on continue(). */
  metadata?: Record<string, unknown>;
  /** Optional ad-hoc per-run wall-clock budget (seconds) for a CODE-JS agent,
   *  overriding the agent's `run_timeout_seconds` and the sidecar's global
   *  default (precedence: per-run > per-agent > global). Use it for a fan-out
   *  orchestrator that blocks in Agent.parallel_spawn awaiting LLM children —
   *  its budget spans that wait, so the CPU-oriented default is often too low.
   *  Ignored by LLM agents. 0 / omitted = inherit. */
  runTimeoutSeconds?: number;
  /** Per-run LLM sampling override (v0.28.0), merged PER FIELD over the
   *  agent's own sampling (this wins; unset fields inherit). Omitted =
   *  inherit entirely. */
  sampling?: SamplingOptions;
  /** Per-run tool choice (RFC DI): whether and which tool the model must
   *  call, and for how many calls. REPLACES the agent's own whole. */
  toolChoice?: ToolChoiceOptions;
  /** Per-run context-compaction override (v0.32.0), merged PER FIELD over
   *  the agent's own compaction block (this wins; unset fields inherit).
   *  Omitted = inherit entirely. Trigger compaction mid-run with
   *  {@link LoomcycleClient.compactRun}. */
  compaction?: CompactionOptions;
  /** Per-run context-DISTILLATION override, merged PER FIELD over the agent's
   *  own `context` block (this wins; unset fields inherit).
   *
   *  Start-only — see {@link ContextOptions}. This is what varies the
   *  distillation strategy for a run without forking the agent, and outside
   *  `mode: "append"` it, not {@link RunOptions.compaction}, is the block the
   *  runtime reads. */
  context?: ContextOptions;
  /** Per-run context-WINDOW override in tokens (RFC CJ). Wins over the agent's
   *  own `max_context_tokens` when > 0; omitted = inherit it (which itself
   *  defers to the provider/driver default). Distinct from a model's output
   *  cap; primarily for local inference (Ollama num_ctx). */
  maxContextTokens?: number;

  /** RFC AI — start a PERSISTENT interactive run that parks at end_turn
   *  awaiting operator steering instead of terminating. The stream emits an
   *  `awaiting_input` frame when it parks; drive it with
   *  {@link LoomcycleClient.sendRunInput}, re-attach with
   *  {@link LoomcycleClient.streamRunByID}, and `cancelAgent` to end it.
   *  Higher-level: {@link LoomcycleClient.interactiveSession}. */
  interactive?: boolean;
  /** Opt-in observability: when true, the iterator emits client-
   *  synthesized `{ type: "_meta", meta_subtype: "stream_open" | "stream_close" }`
   *  events around the real event stream. `meta_reason` carries the
   *  trigger ("eof", "abort", or an error class name). Default is
   *  false — existing consumers see no behaviour change. Useful for
   *  n8n nodes that want to surface "stream re-opened" / "stream
   *  closed" log entries without inferring from event timing. */
  debug?: boolean;
  signal?: AbortSignal;
}

/** Per-run LLM sampling override (v0.28.0). Mirrors config.Sampling — every
 *  field optional; an unset field inherits the agent's value. An explicit
 *  `temperature: 0` is deterministic, NOT "unset". Each provider maps what it
 *  supports (e.g. topK is Anthropic/Gemini/Ollama; frequencyPenalty/
 *  presencePenalty/seed are OpenAI/DeepSeek/Ollama). */
/** Whether and which tool the model must call, and for how many calls
 *  (RFC DI). Same shape per-agent (AgentDef `tool_choice`) and per-run; a
 *  per-run value REPLACES the agent's whole. A model that cannot enforce it
 *  runs anyway, with a `capability_inert` event naming what was not enforced. */
export interface ToolChoiceOptions {
  /** `auto` (provider default), `none` (no tool calls), `required` (some
   *  tool), `tool` (the tool named in `name`). */
  mode: "auto" | "none" | "required" | "tool";
  /** The tool to call — required with mode `tool`, refused otherwise. */
  name?: string;
  /** How long it applies: `first_call` (default), `until_called` (until the
   *  model makes a call that satisfies it), `always` (refused for `required`
   *  and `tool`, which could then never finish). */
  until?: "first_call" | "until_called" | "always";
}

export interface SamplingOptions {
  temperature?: number;
  topP?: number;
  topK?: number;
  frequencyPenalty?: number;
  presencePenalty?: number;
  seed?: number;
  stop?: string[];
}

/** Per-run context-compaction override (v0.32.0). Mirrors config.Compaction —
 *  every field optional; an unset field inherits the agent's value. */
export interface CompactionOptions {
  /** Turn AUTO-compaction on for this run (default off). */
  enabled?: boolean;
  /** Summary aims for ~N% of the compacted span's length (10..50; default 10). */
  targetPercentage?: number;
  /** Keep the last N messages verbatim (default 4; 0 = summarize all). */
  keepLastN?: number;
  /** Pin the first user message (the task) verbatim (default true). */
  keepFirst?: boolean;
  /** Auto-compact when used/window ≥ N% (50..95; default 80; only when
   *  enabled + the provider reports a context window). */
  autocompactAtPct?: number;
  /** Run the summary call on a cheaper/faster model served by the SAME
   *  provider. Omitted = the run's model. */
  model?: string;
}

/** Per-run context-distillation override. Mirrors the server's `context`
 *  block — every field optional; an unset field inherits the agent's value,
 *  merged per field.
 *
 *  DISTINCT from {@link CompactionOptions}, and the two are not
 *  interchangeable: `compaction` is the append-mode summariser, while this
 *  chooses HOW history is distilled at all. Outside `mode: "append"` the
 *  compaction knobs are never consulted — so setting `autocompactAtPct` on a
 *  recap or stateful run does nothing. The server reports settings in that
 *  state under `inert` on GET /v1/runs/{id}/effective-config.
 *
 *  START-ONLY. These are accepted when a run BEGINS, not by retune: `mode` is
 *  latched before the loop starts (the stateful branch is taken or not, and
 *  the tool catalogue is already resolved), so a retune could apply the
 *  thresholds and silently ignore the mode. */
export interface ContextOptions {
  /** How history is distilled.
   *  - `append`   — keep everything; the compaction knobs apply here and
   *                 ONLY here.
   *  - `recap`    — fold the evicted span into a running progress note.
   *  - `stateful` — a different loop: the model emits a patch + action each
   *                 step and carries state rather than transcript.
   *  - `auto`     — resolved at run start from the provider: a local backend
   *                 or an interactive run takes `recap`, a frontier API takes
   *                 `stateful`. */
  mode?: "append" | "recap" | "stateful" | "auto";
  /** Keep the last N messages verbatim (default 6; 0 = distil all).
   *
   *  ⚠️ This is a FLOOR on what can be distilled: a conversation of N+1
   *  messages or fewer has nothing left after the pinned first turn, so it
   *  never distils however full the window is. A chat of few enormous turns
   *  is exactly that shape. The run reports it as a `context_distill_declined`
   *  event with reason `split_declined`, carrying both numbers. */
  keepLastN?: number;
  /** What happens to the evicted span in recap mode.
   *  - `recap` (default) — summarise it into a running note.
   *  - `drop`            — discard it with no note.
   *  - `keep`            — distil nothing. Reported as a `reasoning_keep`
   *                        decline WHEN THE THRESHOLD IS REACHED, so the run
   *                        says why the window is not being reclaimed. Below
   *                        the threshold there is nothing to report and no
   *                        frame is emitted — absence of a decline means the
   *                        gate did not open, not that distillation is
   *                        broken. */
  reasoning?: "recap" | "drop" | "keep";
  /** Character budget for the running recap note (default 512) — a bound on
   *  the note's LENGTH.
   *
   *  ⚠️ It does not decide whether the summariser can answer. A reasoning
   *  model can spend its output budget thinking and return nothing at all,
   *  which the run reports as a `context_distill_declined` event with reason
   *  `empty_summary`; the fix for that is {@link ContextOptions.model}, not a
   *  longer note. */
  recapMaxChars?: number;
  /** Run the RECAP call on a different model, served by the SAME provider.
   *  Unset = the run's own model. Mirrors `compaction.model`.
   *
   *  ⚠️ What this is for is the model's BEHAVIOUR, not its context window. A
   *  recap reads a span of transcript and writes a short note, so it is never
   *  the call that runs out of window; what breaks it is a reasoning model,
   *  which spends its output budget thinking before it writes. Point this at a
   *  cheap non-thinking model beside a thinking chat model. */
  model?: string;
  /** Auto-distil when used/window ≥ N% (50..95; default 80). This is the live
   *  threshold in recap mode — NOT `compaction.autocompactAtPct`. */
  autorecapAtPct?: number;
  /** JSON Schema the stateful mode validates every state patch against.
   *  Stateful mode only. */
  stateSchema?: Record<string, unknown>;
  /** What a stateful run does with a patch that fails the schema
   *  (`retry` default, or `fail`). */
  onInvalidPatch?: "retry" | "fail";
  /** How many times a rejected patch may be retried (default 2). */
  maxPatchRetries?: number;
  /** Grant the Recall tool so the agent can fetch back detail the
   *  distillation dropped. */
  recall?: boolean;
  /** Bank each evicted span for the memory consolidator.
   *
   *  This is the flag the recap and stateful paths actually read —
   *  `compaction.memoryFlush` installs the banking callback but nothing
   *  outside append mode calls it. */
  harvestToMemory?: boolean;
}

/** Opaque caller-tracking lineage (v0.12.x) attached to a run and
 *  propagated to all its sub-agents. The runtime stores and echoes
 *  these fields verbatim and never interprets them. All fields
 *  optional; an all-empty object is treated as absent. */
export interface ParentContext {
  /** The consumer's identifier for the user-initiated run at the root of
   *  the spawn tree — echoed on every descendant. */
  root_agent_run_id?: string;
  /** The consumer's logical-operation key for the root request. */
  function_key?: string;
  /** The consumer's tier marker captured at root-run time (distinct from
   *  `userTier`, which is loomcycle's resolver policy). */
  tier_at_run?: string;
  /** Workflow-board correlation (loomboard, RFC BT P4). A board-bound
   *  `TeamDef op=run` stamps these onto each handler run it spawns, so a client
   *  folding the run-state stream (RunStateEvent.parent_context) can pin the
   *  live agent to its kanban card. Absent for non-board runs. */
  board_scope?: string;
  board_chunk_id?: string;
  board_document_id?: string;
  /** Team-walk correlation (loomcycle v1.78.0). A run spawned by a team walk
   *  carries the walk's own run_id here, plus which WAVE of that walk it
   *  belongs to and its index within the wave.
   *
   *  `walk_id` is what {@link StreamUserRunStatesOptions.walkId} filters on
   *  server-side; `wave_id` + `wave_index` are what let a live view place an
   *  agent within the walk rather than merely inside it — a fan-out of N
   *  agents shares one `wave_id` and differs only by `wave_index`.
   *
   *  Absent on any run no team walk spawned. */
  walk_id?: string;
  wave_id?: string;
  /** Position within the wave, 0-based. Sent even when 0 (unlike the string
   *  fields, which are omitted when empty), so an index of 0 is a real first
   *  position and not an absent one. */
  wave_index?: number;
}

export interface ContinueOptions extends RunOverrideOptions {
  /** Required — the session to continue. */
  sessionId: string;
  segments: PromptSegment[];
  tools?: string[];
  /** Per-call URL allowlist. Same three-state semantics as
   *  RunOptions.allowedHosts — continuations re-supply the list each
   *  time rather than inheriting from the seed run. */
  allowedHosts?: string[] | null;
  /** Brave-side filtering when allowedHosts is set. See RunOptions. */
  webSearchFilter?: "drop" | "keep";
  /** Pin the continuation to a specific running agent_id.
   *  Optional — when set, the server validates that the agent is
   *  live for this session before accepting; rejects with
   *  AgentNotFoundError / SessionBusyError otherwise. Mirrors
   *  Python adapter's ContinueOptions.agent_id (the Python field
   *  is snake_case; the wire field server-side is `agent_id`). */
  agentId?: string;
  /** Per-call user tier (v0.8.2+). Unlike user_id (session-bound),
   *  user_tier is per-request so a user upgrading mid-session sees
   *  the new tier applied to the next continuation. */
  userTier?: string;
  /** Per-call MCP bearer (v0.8.x+). Per-request (not session-bound)
   *  so different continuations in the same session may carry
   *  different end-user tokens. */
  userBearer?: string;
  /** Per-tool named credentials map (v1.x RFC F). See
   *  {@link RunOptions.userCredentials} for the full shape — same
   *  semantics, supplied per-continuation rather than per-fresh-run. */
  userCredentials?: Record<string, string>;
  /** Opaque caller-tracking lineage (v0.12.x). See
   *  {@link RunOptions.parentContext} — same shape; a continuation may
   *  (re)set the lineage for the new run it creates. */
  parentContext?: ParentContext;
  /** Optional NON-SECRET structured metadata for the new run — see
   *  {@link RunOptions.metadata}. Same shape + trust posture. NOT inherited
   *  from the original run (metadata is a per-call input, not session state):
   *  re-send it on the continuation to carry it forward. */
  metadata?: Record<string, unknown>;
  /** Optional ad-hoc per-run code-js wall-clock budget (seconds) for the
   *  continuation's new run — see {@link RunOptions.runTimeoutSeconds}. */
  runTimeoutSeconds?: number;
  /** Per-continuation LLM sampling override — see {@link RunOptions.sampling}. */
  sampling?: SamplingOptions;
  /** Per-run tool choice (RFC DI): whether and which tool the model must
   *  call, and for how many calls. REPLACES the agent's own whole. */
  toolChoice?: ToolChoiceOptions;
  /** Per-continuation context-compaction override — see {@link RunOptions.compaction}. */
  compaction?: CompactionOptions;
  /** Per-continuation context-distillation override — see
   *  {@link RunOptions.context}. */
  context?: ContextOptions;
  /** Per-continuation context-WINDOW override in tokens — see
   *  {@link RunOptions.maxContextTokens}. */
  maxContextTokens?: number;
  /** RFC AI — park this continuation at end_turn for operator steering. See
   *  {@link RunOptions.interactive}. */
  interactive?: boolean;
  /** Opt-in observability: see {@link RunOptions.debug}. Same shape. */
  debug?: boolean;
  signal?: AbortSignal;
}

export interface ClientOptions {
  /** Base URL of the sidecar, e.g. "http://127.0.0.1:8787". Defaults to
   *  http://127.0.0.1:8787; in production, callers pass the deployed
   *  URL (LOOMCYCLE_BASE_URL equivalent). */
  baseUrl?: string;

  /** Bearer token for the Authorization header. Optional in
   *  open-mode deployments (LOOMCYCLE_AUTH_TOKEN unset on the server).
   *  When present, attached as `Authorization: Bearer <token>` on
   *  every request. */
  authToken?: string;

  /** Custom fetch implementation; defaults to global `fetch`.
   *  Useful for testing (vi.fn()) or for runtimes that ship their
   *  own fetch (e.g. node-fetch on older Node). */
  fetch?: typeof fetch;
}

// ---- Agent metadata ----

export type AgentStatus = "running" | "completed" | "failed" | "cancelled";

export interface AgentUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_creation_tokens?: number;
  cache_read_tokens?: number;
  model?: string;
  // provider is the provider id that actually served the final
  // successful iteration (e.g. "anthropic", "deepseek"). Distinct
  // from model so consumers can tell primary-provider runs from
  // runtime-fallback routed runs. Added in v0.12.7 — pre-migration
  // server rows + pre-call failures omit this field.
  provider?: string;
}

export interface Agent {
  agent_id: string;
  run_id: string;
  session_id: string;
  agent: string;
  parent_agent_id: string | null;
  user_id: string;
  status: AgentStatus;
  started_at: string;
  completed_at: string | null;
  stop_reason: string | null;
  error: string | null;
  usage: AgentUsage;
  last_heartbeat_at: string | null;
  live: boolean;
  /** Opaque caller-tracking lineage this run carries (inherited from its
   *  root for sub-agents), v0.12.x. Echoed here alongside `usage` so you
   *  can attribute a child sub-agent's cost to the user-initiated request
   *  in a single fetch. Omitted when the run carried no context. */
  parent_context?: ParentContext;
  /** The run's answer (RFC DI) — what the row had no field for; `stop_reason`,
   *  `error` and `usage` are above. Present on {@link LoomcycleClient.getAgent}
   *  only (listings omit it), and only once the run has finished with
   *  something to report. */
  result?: RunResult;
}

/** A finished run's answer (RFC DI). */
export interface RunResult {
  /** The text of the run's last assistant turn. */
  final_text?: string;
  /** The final structured state of a stateful run. */
  state?: Record<string, unknown>;
}

export interface ListAgentsResponse {
  agents: Agent[];
}

export interface CancelAgentResult {
  /** Number of agents marked cancelled (root + descendants reached
   *  via parent_agent_id cascade). 0 when the agent had already
   *  terminated; the call still succeeds (idempotent contract). */
  cancelledCount: number;
}

// ---- Fan-out (RFC Y) + compaction ----

/** Options for {@link LoomcycleClient.spawnRunBatch} — the RFC Y external
 *  fan-out. Each spawn is a fresh-run {@link RunOptions} (its `sessionId` is
 *  ignored — batch children never continue a session; `signal`/`debug` are
 *  per-call client concerns and not sent). Capped at 32; the server rejects an
 *  over-cap batch. */
export interface RunBatchOptions {
  spawns: RunOptions[];
  /** "join" (default) — block until all children settle, returning the
   *  combined envelope. "detach" (async run handles) is reserved for a future
   *  release and rejected by the server today. */
  mode?: "join";
  /** Optional join deadline (ms): a child still running when it elapses is
   *  cancelled and reported with a cancelled status in-envelope. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

/** One child run's outcome in a batch. Mirrors the server's SpawnResult wire
 *  shape; a per-child failure is reported via `status` + `error`, never as a
 *  thrown error (the batch as a whole still resolves). */
export interface SpawnRunResult {
  agent_id: string;
  run_id: string;
  session_id: string;
  status: AgentStatus;
  stop_reason?: string;
  final_text?: string;
  usage?: AgentUsage;
  error?: string;
}

/** Result of {@link LoomcycleClient.spawnRunBatch} — `results` is index-aligned
 *  with the request's `spawns`. */
export interface RunBatchResult {
  results: SpawnRunResult[];
  spawned: number;
}

/** Result of {@link LoomcycleClient.compactRun}. `applied` is "live" (pushed to
 *  the running loop), "marker" (persisted for a terminal run's next
 *  continuation), or "noop" (too short to compact). */
/**
 * One DERIVED user (RFC-free: a user is a GROUP BY over run activity, not a
 * stored record — so there is no profile here and no way to create one).
 */
export interface DirectoryUser {
  subject: string;
  running_count: number;
  total_count: number;
  /** RFC3339 UTC; absent when the subject has never started a run. */
  last_started_at?: string;
}

/**
 * Token budget for a subject. Both bounds are OPTIONAL because absent and zero
 * differ: absent is "no ceiling at this tier", zero would refuse every run.
 */
export interface DirectoryBudget {
  soft_limit?: number;
  hard_limit?: number;
}

/**
 * One subject's aggregate view (loomcycle v1.46.0+) — the five surfaces an
 * operator otherwise visits separately.
 *
 * `documents` is absent rather than 0 when SQL Memory is not configured: the
 * plane was NOT examined, which is a different statement from empty. Likewise a
 * non-empty `errors` means a plane could not be read, so every count is a LOWER
 * BOUND — do not present the numbers as complete.
 */
export interface DirectoryInspection {
  tenant: string;
  subject: string;
  activity: DirectoryUser;
  chats: number;
  /** Per scope, not one number: the subject's own rows and what a shared agent
   * holds ABOUT them are different things. */
  memory: Record<string, number>;
  documents?: number;
  budget?: DirectoryBudget;
  usage: { calls: number; cost: number };
  errors?: string[];
  notes?: string[];
}

/** One tenant with derived counts. */
export interface DirectoryTenant {
  tenant: string;
  users: number;
  runs: number;
}

/**
 * One tier's per-plane counts in an erasure report.
 *
 * A key's PRESENCE means the plane was examined; its value is the row count.
 * Absence means NOT examined — a different statement from zero, and the
 * distinction the whole report turns on.
 */
export interface ErasureTier {
  counts: Record<string, number>;
  total: number;
}

/**
 * What a subject-keyed delete cannot reach: facts ABOUT the subject living in
 * scopes they do not own, found only by tracing provenance from their chats.
 *
 * `rows: 0` with `sessions_examined: 0` means UNDETERMINABLE, not none — there
 * was nothing to trace from. That is the state a subject is left in after an
 * erasure.
 */
export interface ErasureResidue {
  rows: number;
  scopes: string[];
  sessions_examined: number;
  truncated: boolean;
}

/** Result of {@link LoomcycleClient.erasureReport} (RFC BL P5). */
export interface ErasureReport {
  tenant: string;
  subject: string;
  tier1_covered: ErasureTier;
  tier2_uncovered: ErasureTier;
  tier3_residue: ErasureResidue;
  notes: string[];
  /** Planes that could not be READ. Non-empty means every count is a lower bound. */
  errors?: string[];
}

/** Result of {@link LoomcycleClient.erasureExecute} (RFC BL P5). */
export interface ErasureResult {
  tenant: string;
  subject: string;
  dry_run: boolean;
  deleted: Record<string, number>;
  /** Plane -> why it was kept. Never empty. */
  retained: Record<string, string>;
  residue: ErasureResidue;
  errors?: string[];
  notes: string[];
}

export interface CompactRunResult {
  run_id: string;
  compacted: boolean;
  before_tokens: number;
  after_tokens: number;
  applied: "live" | "marker" | "noop";
}

/** Result of {@link LoomcycleClient.replaySession} (RFC BJ Phase 4) — a NEW
 * session bound to the target agent, seeded with the source conversation.
 * Continue `new_session_id` with the normal message/run path; the carried
 * context replays automatically. */
export interface ReplaySessionResult {
  new_session_id: string;
  seed_run_id: string;
  events_copied: number;
  compacted: boolean;
}

/** Result of {@link LoomcycleClient.cancelTurn} (RFC BH) — the current turn was
 *  stopped and the interactive run parked at awaiting_input (session +
 *  transcript intact). This is NOT whole-run cancel ({@link
 *  LoomcycleClient.cancelAgent}). For a team walk `parked` is false: the walk
 *  was ended, not parked. */
export interface CancelTurnResult {
  run_id: string;
  stopped: boolean;
  parked: boolean;
}

// ---- Transcript ----

/** TranscriptEvent — one persisted store.Event from
 *  GET /v1/sessions/{id}/transcript. The server wraps each
 *  providers.Event in {seq, run_id, ts_ns, type, event:{...}}.
 *
 *  `payload` is the v0.9.1 sidecar field carrying the typed body of
 *  events that don't fit the providers.Event union (the first-cycle
 *  `system_prompt` + `user_input` transcript events). Narrow on
 *  `type` to pick the right payload interface — see
 *  {@link SystemPromptPayload} and {@link UserInputPayload}. */
export interface TranscriptEvent {
  seq: number;
  run_id: string;
  ts_ns: number;
  type: string;
  event: unknown;
  /** v0.9.1+ sidecar for typed transcript events:
   *    type === "system_prompt" → SystemPromptPayload
   *    type === "user_input"    → UserInputPayload[]
   *  Absent for events the server hands through via `event`. */
  payload?: SystemPromptPayload | UserInputPayload[] | unknown;
}

/** UserInputPayload mirrors the JSON of one `loop.PromptSegment` —
 *  what the caller supplied as `segments` on POST /v1/runs +
 *  /v1/sessions/{id}/messages. The transcript event's `payload`
 *  field carries the FULL array (`UserInputPayload[]`) because one
 *  call may include multiple segments (system + user prepends, etc.). */
export interface UserInputPayload {
  role: string; // "system" | "user"
  content: Array<{ type: string; text?: string; cacheable?: boolean }>;
}

/** SystemPromptPayload mirrors the v0.9.1 system_prompt transcript
 *  event payload — the resolved system prompt + provenance metadata
 *  so operators can see WHICH AgentDef + WHICH SkillDef rows fed in. */
export interface SystemPromptPayload {
  system_prompt: string;
  /** Empty for yaml-only agents (no AgentDef row). Pinned for
   *  sub-runs spawned via the Agent tool with a def_id. */
  agent_def_id?: string;
  /** skillName → active SkillDef def_id. Only present for skills
   *  whose DB-active row supplied the body; static-fallback skills
   *  are absent. */
  skill_def_ids?: Record<string, string>;
}

export interface TranscriptResponse {
  session: {
    id: string;
    user_id: string;
    agent: string;
    created_at: string;
  };
  events: TranscriptEvent[];
}

// ---- Health ----

export interface HealthResponse {
  ok: boolean;
  commit?: string;
  built?: string;
  uptime_seconds?: number;
  version?: string;
}

// ---- Admin: users list ----

export interface UserSummary {
  user_id: string;
  running_count: number;
  total_count: number;
  last_started_at: string;
  // RFC BX P2a (loomcycle v1.50.0+) — the first-class users-table fields merged
  // over the run-derived activity above. `registered` distinguishes a managed
  // user row (true) from a subject seen only in runs (false); the record fields
  // are empty for the latter. Optional so older servers (which omit them) still
  // typecheck.
  registered?: boolean;
  display_name?: string;
  access_mode?: string; // "tenant" | "isolated"
  status?: string; // "active" | "disabled"
}

export interface ListUsersResponse {
  users: UserSummary[];
}

// ---- RFC BX Phase 2: tenant-owned users + delegated per-user tokens (v1.50.0+) ----

/** One first-class users-table row (POST/PATCH /v1/_users). The tenant is
 *  server-derived from the authenticated principal, never sent. */
export interface UserRecord {
  tenant_id: string;
  subject: string;
  display_name: string;
  access_mode: string; // "tenant" | "isolated"
  status: string; // "active" | "disabled"
  created_at: string;
  created_by: string;
}

export interface CreateUserBody {
  subject: string;
  display_name?: string;
  /** "tenant" (default) collaborates on the tenant's shared primitives;
   *  "isolated" confines the member to its own user scope. */
  access_mode?: string;
  status?: string; // default "active" server-side
}

/** PATCH body — an omitted key leaves the column unchanged; a present key
 *  (even the empty string) is applied. */
export interface UpdateUserBody {
  display_name?: string;
  access_mode?: string;
  status?: string;
}

/** The show-once result of POST /v1/_users/{subject}/tokens. `token` is the
 *  plaintext bearer, returned exactly once and never retrievable again. */
export interface MintedUserToken {
  def_id: string;
  token: string;
  token_suffix: string;
  name: string;
  scopes: string[];
  created_at: string;
  warning: string;
}

/** One token row from GET /v1/_users/{subject}/tokens — METADATA ONLY (never
 *  the plaintext or hash). `active` applies the auth-layer validity rule. */
export interface UserTokenMeta {
  def_id: string;
  name: string;
  scopes: string[];
  created_at: string;
  retired_at?: string;
  active: boolean;
}

export interface ListUserTokensResponse {
  subject: string;
  tokens: UserTokenMeta[];
}

// ---- RFC BY: runnable-agent discovery (v1.51.0+) ----

/** One entry in the runnable-agent catalog — the name to run + its tier.
 *  Intentionally lean: no operator metadata (versions, retired, hashes). */
export interface RunnableAgent {
  name: string;
  source: string; // "bundled" | "tenant" | "own"
}

export interface RunnableAgentsResponse {
  agents: RunnableAgent[];
}

// ---- Whoami / principal (RFC L, v0.17.0) ----

/** GET /v1/_me — the authenticated principal resolved from the bearer.
 *  `open_mode` is true when the server runs without the OperatorTokenDef
 *  substrate (single shared LOOMCYCLE_AUTH_TOKEN); `legacy` is true for a
 *  shared-token principal when the substrate IS enabled. */
export interface WhoamiResponse {
  tenant_id: string;
  subject: string;
  scopes: string[];
  is_admin: boolean;
  legacy: boolean;
  open_mode?: boolean;
}

// ---- Pause / Resume / State (v0.8.17 wire shape, v0.8.18 real impls) ----

export type RuntimeStateStatus = "running" | "pausing" | "paused";

export interface PauseResult {
  state: string; // "paused"
  duration_ms: number;
  force_cancelled_count: number;
  paused_runs_count: number;
  warnings?: string[];
}

export interface ResumeResult {
  state: string; // "running"
  resumed_runs_count: number;
  warnings?: string[];
}

export interface RuntimeStateResponse {
  state: RuntimeStateStatus;
  paused_runs_count: number;
}

// ---- Resolver re-probe (issue #88) ----

export interface ResolverModelStatus {
  listed: boolean;
  stalled: boolean;
}

export interface ResolverProviderAvailability {
  excluded: boolean;
  reachable: boolean;
  models: Record<string, ResolverModelStatus>;
  /** RFC3339 timestamp of the last probe for this provider. */
  last_check: string;
  last_error?: string;
}

/** The resolver availability matrix, captured right after a forced
 *  re-probe. Same shape as GET /v1/_resolver. */
export interface ResolverMatrix {
  /** RFC3339 timestamp when this matrix snapshot was assembled. */
  generated_at: string;
  providers: Record<string, ResolverProviderAvailability>;
}

// ---- Snapshot ----

export interface SnapshotDescriptor {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
}

export interface SnapshotListResponse {
  entries: SnapshotDescriptor[];
}

export interface SnapshotCreateResponse {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
}

/** Full envelope returned by GET /v1/_snapshots/{id}. json_content
 *  carries the canonical envelope as a parsed JSON object — pipe
 *  it directly into restoreSnapshot's `json` field. */
export interface SnapshotEnvelope {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
  json_content: unknown;
}

export interface CreateSnapshotOptions {
  /** Free-text marker stored on the snapshots.label column. */
  label?: string;
  /** Capture the optional interaction_history section (large; opt-in). */
  includeHistory?: boolean;
  /** RFC3339 timestamp; only honoured when `includeHistory` is true. */
  includeHistorySince?: string;
  /** Override the operator's LOOMCYCLE_SNAPSHOT_MAX_BYTES cap. 0 = default. */
  maxBytes?: number;
}

export interface SnapshotRestoreResponse {
  agent_defs_restored?: number;
  agent_def_active_restored?: number;
  memory_restored?: number;
  channel_messages_restored?: number;
  channel_cursors_restored?: number;
  evaluations_restored?: number;
  paused_runs_restored?: number;
  synthesized_sessions?: number;
  transcript_events_restored?: number;
  interaction_history_restored?: number;
  warnings?: string[];
}

// ---- Memory admin ----

export interface MemoryScopeKind {
  name: string;
  description: string;
}

export interface MemoryScopesResponse {
  scopes: MemoryScopeKind[];
}

export interface MemoryScopeIDSummary {
  scope_id: string;
  key_count: number;
  bytes: number;
  updated_at: string;
}

export interface MemoryScopeIDsResponse {
  scope: string;
  scope_ids: MemoryScopeIDSummary[];
}

export interface MemoryEntry {
  key: string;
  value: unknown;
  expires_at?: string;
  created_at: string;
  updated_at: string;
  /** When the remembered thing was SAID, as distinct from `created_at` (when it
   *  was stored, which on a bulk import is one instant for the whole corpus).
   *  Absent on rows nobody dated, which is most of them. */
  observed_at?: string;
  /** When the thing became TRUE in the world — a third time again, and the one a
   *  question about "what was happening on the 3rd" is really asking about.
   *  Half-open with {@link invalid_at}: absent `invalid_at` means still true. */
  valid_at?: string;
  invalid_at?: string;
}

export interface MemoryEntriesResponse {
  scope: string;
  scope_id: string;
  entries: MemoryEntry[];
  truncated: boolean;
}

export interface MemoryEntryResponse {
  scope: string;
  scope_id: string;
  entry: MemoryEntry;
}

// ---- RFC BV memory-view: off-run unified search + embed-admin reads ----

/** Input for {@link LoomcycleClient.memorySearch}. camelCase here; the
 *  client maps to the snake_case wire body (scopeId→scope_id,
 *  topK→top_k). `rank` / `dedup` are opaque ranking-config objects
 *  passed through to the server as-is. */
export interface MemorySearchInput {
  query: string;
  scope: string;
  scopeId: string;
  topK?: number;
  rank?: Record<string, unknown>;
  dedup?: Record<string, unknown>;
  /** Which kinds of remembered thing to return (RFC BW). Omit to span every
   *  plane — this endpoint exists to answer "where did I record this", so
   *  unlike the in-band `recall` it does not default to facts.
   *
   *  Mixing `"documents"` with only ONE of `"facts"`/`"notes"` is rejected
   *  (400 `invalid_sources`): the namespace and the provenance split are
   *  independent dimensions, so that combination would need a disjunction the
   *  store does not build. Use `["facts"]`, `["notes"]`,
   *  `["facts","notes"]`, `["documents"]`, or all three. */
  sources?: MemorySource[];
  /** Narrow by WHEN the remembered thing was said — its `observed_at` — rather
   *  than when it was stored. Omit for no time constraint.
   *
   *  **Give a generous window.** A remark about an event is normally made AFTER
   *  it: measured on a conversational corpus the lag runs -3 to +10 days, median
   *  +1, so an unwidened one-day window contains the row you want in about one
   *  case in eight. `slack` (default `"3d"`) widens both bounds and is what makes
   *  a narrow window usable.
   *
   *  `missing` decides what happens to rows nobody dated: `"prefer"` (the default
   *  when a window is given) drops NOTHING and merely promotes in-window rows,
   *  while `"require"` is a real filter. A tight `"require"` window is how a
   *  scope that holds the answer comes back empty. */
  when?: MemoryWhen;
}

/** The observed-time predicate on {@link MemorySearchInput}. Timestamps are
 *  RFC3339 instants — resolve phrases like "the first weekend of October"
 *  yourself; the server takes instants, not prose, and a malformed one is
 *  rejected rather than ignored. */
export interface MemoryWhen {
  from?: string;
  to?: string;
  /** How far outside the bounds still counts, e.g. `"3d"` or `"72h"`. Default `"3d"`. */
  slack?: string;
  missing?: "prefer" | "require";
  /** Asks a DIFFERENT question from `from`/`to`: not when something was said, but
   *  what was TRUE at this instant — matching the row's `valid_at`/`invalid_at`
   *  interval. The two compose, so "what did we learn in November about what was
   *  true in late October" is one query.
   *
   *  Always a hard filter, unlike the observed window: a row valid over a
   *  different interval is not a weaker answer to "what was true then", it is a
   *  wrong one. `missing` still decides whether undated rows survive. */
  as_of?: string;
}

/** A kind of remembered thing (RFC BW).
 *
 *  - `facts` — memory a consolidator distilled; provenance is server-stamped.
 *  - `notes` — memory an agent wrote directly with `set`.
 *  - `documents` — Document chunk bodies, which share the memory keyspace. */
/** Which kinds of remembered thing a search or recall may return.
 *
 *  `"traces"` is raw conversation turns — the material the other three were derived
 *  FROM. It must be asked for ALONE: a combined query would rank a turn against the
 *  fact extracted from it, and an unfiltered search excludes traces entirely so that
 *  adding the class did not change what every existing caller gets back. */
export type MemorySource = "facts" | "notes" | "documents" | "traces";

/** One hit in a {@link MemorySearchResponse}.
 *
 *  `kind` is the row's class: `"fact"` (a consolidator distilled it),
 *  `"note"` (an agent wrote it directly) or `"document"` (a Document chunk
 *  body). A document hit also carries `chunk_id` so the viewer can fetch its
 *  entity block via document({ op: "get_chunk" }), plus `document` and `title`
 *  — the document it came from and the heading it sits under — so a page of
 *  hits can be attributed without one fetch per row.
 *
 *  **Changed in 1.49.0 (RFC BW):** this was `"memory" | "document"`. Code
 *  switching on `"document"` is unaffected; code comparing against
 *  `"memory"` must move to `"fact" | "note"`. The split is the point — "the
 *  user told me this" and "an agent jotted this down" are different claims.
 *
 *  Field casing mirrors the wire JSON (snake_case). */
export interface MemorySearchEntry {
  key: string;
  value: unknown;
  /** raw cosine similarity */
  score: number;
  /** hybrid rank the row was ordered by */
  rank_score: number;
  embedded_with: { provider: string; model: string };
  kind: "fact" | "note" | "document";
  chunk_id?: string;
  /** The document this chunk belongs to, by title. Document hits only, and
   *  best-effort: the titles live in SQL Memory, a different plane from the
   *  bodies, so a deployment without it omits the field. */
  document?: string;
  /** The chunk's own heading. Same conditions as {@link document}. */
  title?: string;
}

export interface MemorySearchResponse {
  scope: string;
  scope_id: string;
  entries: MemorySearchEntry[];
  query_embedding_dim: number;
  truncated: boolean;
  /** What the `when` predicate did — present only when one was sent.
   *
   *  Read it before concluding a scope knows nothing: `in_window: 0` with a large
   *  `untimed` means the corpus is undated, not that the answer is absent, and
   *  those call for very different responses. Counts describe the candidate pool
   *  the ranker saw, not a scope-wide census. */
  time_filter?: MemoryTimeFilter;
}

export interface MemoryTimeFilter {
  mode: "prefer" | "require";
  slack_seconds: number;
  in_window: number;
  out_of_window: number;
  untimed: number;
}

/** One row in {@link MemoryEmbedStatsResponse}.models — the wire shape
 *  of store.MemoryEmbedModelStats. */
export interface MemoryEmbedModelStats {
  provider: string;
  model: string;
  dimension: number;
  row_count: number;
}

export interface MemoryEmbedStatsResponse {
  scope: string;
  models: MemoryEmbedModelStats[];
  total_embedding_bytes: number;
}

/** The configured embedder a reembed call reports back
 *  (current_embedder). */
export interface MemoryReembedConfigured {
  provider: string;
  model: string;
  dimension: number;
}

/** Response to {@link LoomcycleClient.reembedMemory}. A discriminated
 *  union on `dry_run` (mirrors the two server response shapes): a dry
 *  run reports the planned migration WITHOUT writing; a real run
 *  reports what was written. Narrow on `dry_run` before reading the
 *  arm-specific fields. */
export type MemoryReembedResponse =
  | {
      scope: string;
      scope_id: string;
      dry_run: true;
      rows_total: number;
      rows_to_reembed: number;
      current_embedder: MemoryReembedConfigured;
      sample_keys: string[];
      sample_keys_capped: boolean;
    }
  | {
      scope: string;
      scope_id: string;
      dry_run: false;
      rows_reembedded: number;
      rows_failed: number;
      current_embedder: MemoryReembedConfigured;
      failed_keys?: string[];
    };

// ---- Interruption ----

export type InterruptStatus = "pending" | "answered" | "cancelled" | "expired";

export interface InterruptRow {
  interrupt_id: string;
  run_id: string;
  kind: string;
  status: InterruptStatus;
  question?: string;
  options?: string[];
  context_data?: string;
  priority: string;
  answer?: string;
  created_at: string;
  expires_at?: string;
  resolved_at?: string;
  resolved_by?: string;
  user_id?: string;
  agent_id?: string;
  agent_name?: string;
}

export interface InterruptListResponse {
  interrupts: InterruptRow[];
  total: number;
}

export interface ResolveInterruptOptions {
  /** The human's answer. When the original ask declared options,
   *  MUST be one of them (server-side validated). Optional so a
   *  decline ({@link LoomcycleClient.cancelInterrupt}) can omit it. */
  answer?: string;
  /** Audit attribution for who resolved it (free-form). Defaults
   *  server-side to "client" when omitted. */
  resolvedBy?: string;
  /** Discriminator. v0.8.16 supports only "question"; reserved
   *  for v0.9.x future kinds. */
  kind?: string;
  /** Disposition (RFC BH). Omit / "answer" carry the answer; "declined"
   *  resolves the interrupt WITHOUT an answer (skips option validation) so
   *  the waiting Question tool proceeds. */
  disposition?: "answer" | "declined";
}

// ---- Hook management (hooks-connector series, PR C) ----
//
// Hook *registration* is a client concern surfaced here. Hook
// *callback delivery* is server-side: loomcycle POSTs PreHookCall /
// PostHookCall payloads to the consumer's registered callback_url —
// that endpoint is whatever web framework JobEmber / the consumer
// runs. The TS adapter exports the PreHookCall / PostHookCall /
// PreHookResult / PostHookResult shapes so consumers can type their
// receiver code identically to the server's wire emit, but the adapter
// itself never runs them — it only manages the registration.

export type HookPhase = "pre" | "post";

export type HookFailMode = "open" | "closed";

/** Hook is the full descriptor returned by listHooks. The id +
 *  registered_at are loomcycle-assigned; the rest mirrors what was
 *  POSTed to registerHook. Field names use snake_case to match the
 *  Go server's JSON output. */
export interface Hook {
  id: string;
  owner: string;
  name: string;
  phase: HookPhase;
  agents: string[];
  tools: string[];
  callback_url: string;
  fail_mode: HookFailMode;
  timeout_ms: number;
  registered_at: string; // ISO 8601
}

/** RegisterHookOptions uses camelCase (JS norm for method parameters)
 *  and is translated to snake_case in the request body — same split as
 *  RunOptions / CreateSnapshotOptions. */
export interface RegisterHookOptions {
  /** App UID; (owner, name) is the identity tuple. Re-registering the
   *  same pair replaces the prior entry with a fresh id. */
  owner: string;
  name: string;
  phase: HookPhase;
  /** Agent name globs (exact match or trailing-* prefix). Empty list
   *  matches every agent (equivalent to ["*"]). */
  agents?: string[];
  /** Tool name globs (same syntax). Empty matches every tool. */
  tools?: string[];
  /** http:// or https:// URL loomcycle POSTs PreHookCall /
   *  PostHookCall payloads to. */
  callbackUrl: string;
  /** "open" (default) — webhook errors pass through. "closed" — webhook
   *  errors fail the tool call with IsError=true. */
  failMode?: HookFailMode;
  /** Per-call timeout. 0 / omitted = registry default (5 s). */
  timeoutMs?: number;
}

export interface RegisterHookResponse {
  /** Loomcycle-assigned id. Use it on deleteHook. */
  id: string;
}

export interface ListHooksResponse {
  hooks: Hook[];
}

// ---- Hook callback payloads ----
//
// These describe what the consumer's callback_url endpoint RECEIVES
// from loomcycle. The adapter doesn't post these — the operator's
// hooks.Dispatcher does. Exposed here so consumers can type their
// receiver code (e.g. an Express handler, a Next.js route) against
// the same shapes the server emits.

export interface HookToolCall {
  id: string;
  name: string;
  /** Raw JSON the model produced; consumers parse as needed. */
  input: unknown;
}

export interface HookToolResult {
  text: string;
  is_error?: boolean;
}

export interface PreHookCall {
  phase: "pre";
  owner: string;
  hook_name: string;
  agent: string;
  user_id?: string;
  agent_id?: string;
  tool_call: HookToolCall;
}

/** Response a Pre webhook returns. All fields optional; empty body
 *  means "pass through unchanged". See the server-side docs for the
 *  allow_hosts confused-deputy hazard before populating it. */
export interface PreHookResult {
  /** Rewrite the tool input. Tool runs with this instead of the
   *  model's original payload. */
  input?: unknown;
  /** Short-circuit the call: model sees this synthetic result. When
   *  set, allow_hosts is dropped (deny wins; we do not let a denied
   *  hook contribute hostnames to peers in the chain). */
  deny?: HookToolResult;
  /** Per-call host approvals. Only takes effect when the registering
   *  owner is in the operator yaml's hooks.permit_host_widen.owners.
   *  Read the SECURITY note in internal/hooks/types.go before using —
   *  this is a confused-deputy attack surface. */
  allow_hosts?: string[];
}

export interface PostHookCall {
  phase: "post";
  owner: string;
  hook_name: string;
  agent: string;
  user_id?: string;
  agent_id?: string;
  tool_call: HookToolCall;
  tool_result: HookToolResult;
}

// ---- v0.8.22 substrate admin (AgentDef + SkillDef) ----

/** Input shape for {@link LoomcycleClient.agentDef} and
 *  {@link LoomcycleClient.skillDef}. Mirrors the in-process tool
 *  input — `op` discriminates create / fork / get / list / promote
 *  / retire and the remaining fields are op-specific.
 *
 *  Typed loosely because the in-process tool owns the full schema;
 *  the adapter doesn't re-validate. Use the optional `extra` index
 *  signature for forward-compat fields. */
export type SubstrateToolInput = {
  // `delete` + `purge` are the flat VolumeDef lifecycle ops (RFC AH) — a
  // Volume points at mutable on-disk state, so it has no version chain to
  // retire/promote/fork. `delete` unmaps (keeps files); `purge` removes the
  // row AND the directory tree.
  op:
    | "create"
    | "fork"
    | "get"
    | "list"
    | "promote"
    | "retire"
    | "rediscover"
    | "verify"
    | "delete"
    | "purge";
  name?: string;
  def_id?: string;
  parent_def_id?: string;
  overlay?: Record<string, unknown>;
  description?: string;
  promote?: boolean;
  retired?: boolean;
  // VolumeDef create takes a flat `mode` (ro|rw); the runtime derives the
  // path, so callers never supply one.
  mode?: VolumeMode;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.agentDef} and
 *  {@link LoomcycleClient.skillDef}. `unknown` because the shape
 *  varies per op — create/fork return a row envelope, list returns
 *  `{name, versions: [...]}`, promote/retire return summary shapes.
 *  Callers narrow as needed. */
export type SubstrateToolResponse = unknown;

// ---- RFC AP Agent Teams (TeamDef substrate) ----
//
// TeamDefs are agent-team workflow graphs (states + transitions + per-state
// handler agent). The team methods mirror the HTTP shapes in the Web UI's
// api.ts; the substrate is tenant-confined (the runtime stamps the caller's
// authoritative tenant), so a team is visible only within its own tenant.

/** One team's roll-up in {@link ListTeamsResponse} (GET /v1/_teamdef/names). */
export interface TeamNameSummary {
  name: string;
  tenant_id?: string;
  version_count: number;
  active_def_id?: string;
  latest_version: number;
  last_updated?: string;
  live_version_count?: number;
  active_retired?: boolean;
}

/** Response of {@link LoomcycleClient.listTeams}. `names` is `null` (not `[]`)
 *  when the tenant has no teams — mirrors the server's Go nil-slice encoding. */
export interface ListTeamsResponse {
  names: TeamNameSummary[] | null;
}

/** A rendered team diagram from {@link LoomcycleClient.renderTeamDiagram}
 *  (op=render_diagram). `diagram` is Mermaid `stateDiagram-v2` source. */
export interface TeamDiagram {
  name: string;
  def_id: string;
  format: string;
  diagram: string;
}

/** Result of {@link LoomcycleClient.createTeam} / {@link LoomcycleClient.forkTeam}
 *  (op=create / op=fork) — the newly-written version's identifiers. */
export interface CreatedTeam {
  def_id: string;
  name: string;
  version: number;
}

/** One team version's full record from {@link LoomcycleClient.getTeamDef}
 *  (op=get). `definition` is the stored graph, inlined as an object (the server
 *  stores it as raw JSON), suitable for loading into an editor. */
export interface TeamDefDetail {
  def_id: string;
  name: string;
  version: number;
  retired?: boolean;
  content_sha256?: string;
  definition: unknown;
}

/** One team version's record in {@link TeamVersionList} (op=list). Same shape
 *  as {@link TeamDefDetail} plus the lineage + authorship fields the version
 *  history needs — who wrote it, when, and which version it forked from. */
export interface TeamVersion {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at?: string;
  created_by_agent_id?: string;
  retired?: boolean;
  bootstrapped_from_static?: boolean;
  content_sha256?: string;
  definition?: unknown;
}

/** Result of {@link LoomcycleClient.listTeamVersions} (op=list) — every version
 *  of ONE team, newest first. Tenant-scoped server-side. */
export interface TeamVersionList {
  name: string;
  versions: TeamVersion[];
}

/** Result of {@link LoomcycleClient.promoteTeam} (op=promote) — the version the
 *  active pointer now names. */
export interface PromotedTeam {
  def_id: string;
  name: string;
  promoted: boolean;
}

/** Result of {@link LoomcycleClient.retireTeam} (op=retire) — the version's new
 *  retired state. Retiring is reversible (pass `false` to un-retire); it is
 *  {@link LoomcycleClient.deleteTeam} that removes anything. */
export interface RetiredTeam {
  def_id: string;
  retired: boolean;
}

/** Result of {@link LoomcycleClient.verifyTeam} (op=verify) — whether a locally
 *  computed content hash matches the deployed active version.
 *
 *  `deployed: false` means the name has no active version in this tenant at all
 *  (`matches` is then false and the current_* fields are empty) — distinct from
 *  a deployed version whose hash differs, which is a DRIFT rather than an
 *  absence. */
export interface TeamVerification {
  name: string;
  matches: boolean;
  deployed: boolean;
  current_sha256: string;
  current_def_id: string;
  version: number;
}

/** Result of {@link LoomcycleClient.runTeam} (op=run) — the walk trace. `status`
 *  is `"completed"` (a terminal state was reached) or `"iteration_cap"` (a
 *  state's cycle cap tripped; `capped_state` + `iteration_count` describe it).
 *  `steps` is the per-state trace. Extra fields are tolerated (the run output
 *  varies by outcome), so callers narrow as needed. */
export interface TeamRunResult {
  name: string;
  def_id: string;
  status: string;
  /** The walk's own run id. A team walk IS a run — it opens a `runs` row filed
   *  under `team:<name>` — which is what makes it addressable while it runs:
   *  {@link LoomcycleClient.setRunBreakpoints} arms it, `GET /v1/runs/{id}/
   *  interrupts` carries a pause, and cancel stops it. Returned on the
   *  synchronous path too, so a caller holding a second connection can debug a
   *  walk it is waiting on. */
  run_id?: string;
  final_state?: string;
  final_output?: string;
  capped_state?: string;
  max_iterations?: number;
  iteration_count?: number;
  steps: Array<Record<string, unknown>>;
  [extra: string]: unknown;
}

/** What team {@link LoomcycleClient.runTeam} walks, and how. */
export interface TeamRunTarget {
  name?: string;
  defId?: string;
  /** The initial task handed to the entry state's agent. */
  input?: string;
  /** Bind the walk to a Document chunk task board: each state transition
   *  persists `chunk.status` = the current team state, and every handler run
   *  the walk spawns carries the task key on its `parent_context`. */
  boardChunkId?: string;
  /** The Document scope of `boardChunkId` (agent | user, default user). */
  boardScope?: "agent" | "user";
  /** "detach" returns the run id immediately and leaves the walk running
   *  behind it. Omit to wait for the walk and get its trace — which still
   *  returns `run_id`, so a second connection can debug a walk you await. */
  mode?: "detach";
  /** Starter state ids to pause at, armed before the walk starts. Each is
   *  `"<state>"` (both phases) or `"<state>:before_dispatch"` /
   *  `"<state>:after_collection"`.
   *
   *  A walk can also be armed AFTER it starts — see
   *  {@link LoomcycleClient.setRunBreakpoints} — which is the case this
   *  argument cannot serve: you start a run expecting it to work, watch a wave
   *  go wrong, and want to stop before the next one. */
  breakpoints?: string[];
}

/** What {@link LoomcycleClient.runTeam} returns for `mode: "detach"` — the
 *  handle, immediately, with the walk still running behind it.
 *
 *  Detaching exists because op=run is otherwise SYNCHRONOUS: the caller learns
 *  nothing until the walk is over, so there is no moment at which it can arm a
 *  breakpoint, read a pause, or watch progress. There are no `steps` yet —
 *  poll the run, or read its events, for those. */
export interface TeamRunDetached {
  name: string;
  def_id: string;
  /** Address every other run surface with this. */
  run_id: string;
  /** Always "running" — the walk has been started, not awaited. */
  status: string;
  [extra: string]: unknown;
}

/** The armed debug breakpoints of a live team walk, as
 *  {@link LoomcycleClient.getRunBreakpoints} / {@link LoomcycleClient.setRunBreakpoints}
 *  report them. */
export interface TeamBreakpoints {
  run_id: string;
  /** Canonical: always phase-qualified (`"<state>:before_dispatch"`) and
   *  sorted, so what you read back is what the walk will actually do rather
   *  than an echo of the shorthand you sent. */
  armed: string[];
}

/** Input for {@link LoomcycleClient.path} — the RFC AL Unix-like VFS tool
 *  (POST /v1/_path). Op-discriminated; the server resolves scope + tenant
 *  from the authenticated principal, never the wire. Address Memory entries,
 *  Volume mounts, and Documents by human-readable paths (e.g. /docs/launch). */
export type PathToolInput = {
  op: "resolve" | "ls" | "stat" | "mkdir" | "mv" | "rm";
  /** Absolute path, e.g. /docs/launch. Segments are [a-zA-Z0-9._-]; no "..". */
  path?: string;
  /** Destination path (mv only). */
  to?: string;
  /** Which tree (default agent). user needs a user on the run; tenant is shared. */
  scope?: "agent" | "user" | "tenant";
  /** ls: list descendants. rm: required to remove a path that has descendants. */
  recursive?: boolean;
  /** ls: only entries of this kind (document/volume_mount/memory_entry/directory). */
  kind_filter?: string;
  /** ls: maximum entries to return (default 500, max 5000). A truncated listing
   *  reports `truncated: true` and a `next_cursor`; pass that back as `cursor`. */
  limit?: number;
  /** ls: continue a truncated listing with the previous response's `next_cursor`.
   *  Opaque — its encoding is not part of the contract, so do not construct one. */
  cursor?: string;
  /** rm: also delete the backing resource (NOT supported in v1). */
  resource_too?: boolean;
  [extra: string]: unknown;
};

/** Input for {@link LoomcycleClient.document} — the RFC AK chunked-graph
 *  Document tool (POST /v1/_document). Op-discriminated; requires SQL Memory on
 *  the sidecar.
 *
 *  Scope agent/user/**tenant**. Tenant was "deferred" here long after the runtime
 *  gained it (v1.41.0), and the omission was not merely stale documentation: this
 *  type is what the Web UI's document viewer narrows against, so `"tenant"` had
 *  nowhere to go and collapsed to `"user"` — every tenant-scope document listed in
 *  the Path tree and then opened with no chunks. A tenant document additionally
 *  requires the operator to grant BOTH `memory_scopes` and `sql_scopes` with
 *  `tenant`, since a document spans both planes. */
export type DocumentToolInput = {
  // Op order mirrors the backend enum (internal/tools/builtin/document.go)
  // exactly so a reader can line the two up 1:1. set_path attaches/re-homes a
  // Path-tree name for an existing document; export_md / import_md render to /
  // build from export_md-shaped Markdown.
  op:
    | "create_document"
    | "get_document"
    | "documents_summary"
    | "query_documents"
    | "delete_document"
    | "set_path"
    | "create_chunk"
    | "upsert_chunk"
    | "get_chunk"
    | "update_chunk"
    | "delete_chunk"
    | "supersede_chunk"
    | "graph_recall"
    | "move_chunk"
    | "reorder_chunk"
    | "link_chunks"
    | "unlink_chunks"
    | "get_edges"
    | "query_chunks"
    | "add_tags"
    | "remove_tags"
    | "list_tags"
    | "define_type"
    | "list_types"
    | "set_asset"
    | "get_asset"
    | "export_md"
    | "import_md"
    // RFC BS — document connections (backlinks / vector-related / unlinked
    // mentions), per-chunk edit history (history / get_version / diff), and the
    // node-edge canvas graph (export_canvas / import_canvas). Appended (not
    // interleaved) because these were added to the backend enum after the ops
    // above; the union order is only for type-completeness — the wire passthrough
    // carries any `op` verbatim.
    | "backlinks"
    | "related"
    | "unlinked_mentions"
    | "history"
    | "get_version"
    | "diff"
    | "export_canvas"
    | "import_canvas"
    // The fact tier (RFC CC). upsert_chunk/supersede_chunk/graph_recall are above;
    // these are the rest of it.
    | "list_facts"
    | "judge_fact"
    | "verbatim_answer"
    | "verification_stats"
    | "remember"
    | "propose_entity"
    | "propose_subject"
    | "search"
    // Remote document sources (RFC CE).
    | "set_remote"
    | "sync"
    | "diff_remote";
  scope?: "agent" | "user" | "tenant";
  /** Document id (get/delete_document) or chunk id (get/update/delete/move_chunk). */
  id?: string;
  /** create_document: name the doc in the Path tree; get/delete: address by path. */
  path?: string;
  title?: string;
  document_id?: string;
  parent_id?: string;
  new_parent_id?: string;
  type?: string;
  body?: string;
  fields?: Record<string, unknown>;
  status?: string;
  /** The chunk's or document's tags (RFC BS). create/update/upsert_chunk +
   *  create_document replace-set the whole set (omit = unchanged, [] = clear);
   *  add_tags/remove_tags take the tags to change. Nested tags use a slash. */
  tags?: string[];
  /** query_chunks / query_documents: return only items carrying exactly this tag. */
  tag?: string;
  /** query_chunks: match this tag OR anything nested under it (prefix + '/'). */
  tag_prefix?: string;
  position?: number;
  /** update_chunk: the chunk's current revision (optimistic concurrency).
   *  get_version: the historical revision to fetch. */
  revision?: number;
  /** diff (RFC BS): the two chunk revisions to compare (from_revision →
   *  to_revision), producing a unified-diff text. */
  from_revision?: number;
  to_revision?: number;
  /** import_canvas (RFC BS): a node/edge canvas graph to build a document from
   *  (the shape export_canvas emits). Opaque here; the backend owns the schema. */
  canvas?: unknown;
  from_id?: string;
  to_id?: string;
  kind?: string;
  /** query_chunks: restrict to documents at/under this Path-tree path. */
  under_path?: string;
  /** query_chunks: raw read-only SELECT (escape hatch; validator-gated). */
  sql?: string;
  /** Row/response bound. On `documents_summary` it defaults to 500 (max 5000) and
   *  the response reports `truncated: true` when it clips — an `under_path` over a
   *  subject-homed fact store is as wide as the tenant's entity count, so page the
   *  directory with `path op=ls` and pass each page's ids as `document_ids`. */
  limit?: number;
  /** define/list_types: the type name. */
  name?: string;
  /** export_md: embed round-trippable chunk metadata + edges as HTML comments
   *  (default true server-side). false = clean human-facing Markdown. */
  include_metadata?: boolean;
  /** search / graph_recall: free text. search matches it semantically against
   *  chunk BODIES; graph_recall uses it to find starting chunks by title.
   *  verbatim_answer: the lookup question to answer. */
  query?: string;
  /** upsert_chunk: the stable identity of this fact — upserting twice with the
   *  same key updates ONE chunk. judge_fact: name the fact by key instead of id. */
  natural_key?: string;
  /** The entity this fact is about, paired with `type` (RFC CC). Documents
   *  carry neither, which is what lets the ontology gate tell them apart. */
  subject?: string;
  /** upsert_chunk: the span of source text this fact was drawn from — the
   *  evidence a judge checks the claim against. */
  source_quote?: string;
  /** judge_fact: whether the fact's recorded span supports it. The confidence
   *  each maps to is set by the SERVER, so a caller cannot invent a scale.
   *  `mistyped` = the span does support the claim, but it is filed as the wrong
   *  kind of thing; the fix is a retype, so it stays visible. */
  verdict?: "supported" | "unclear" | "unsupported" | "mistyped";
  /** judge_fact: why. Required — a withheld fact whose ground is not stated is
   *  indistinguishable from a bug. */
  reason?: string;
  /** list_facts / graph_recall: also return facts a judge marked unsupported.
   *  They are withheld by default and never deleted, so this is how to read what
   *  was refused, and why. */
  include_refuted?: boolean;
  /** list_facts: return only CLAIMS, dropping the entity identity nodes (the subject
   *  a fact is about — "Ollama", "the user"). A claim carries a sentence and can carry
   *  a source span; an identity node is a name and never can. Off by default because
   *  the plain listing is every chunk carrying entity metadata, which is what document
   *  sync reconciles — so a reading or coverage surface wants this on, and a
   *  federation one does not. */
  claims_only?: boolean;
  /** list_facts: the facts about ONE SUBJECT — the subject's entity chunk id (a
   *  subject document's `root_chunk_id`, from get_document). Returns the facts filed
   *  under it AND the facts filed elsewhere that reference it: filing is
   *  single-parent, so a fact about two things lives in one of their documents and
   *  only points at the other, and a document filter would lose it from the second
   *  subject entirely. */
  about?: string;
  /** remember: a statement to store as a fact that cites ITSELF — the text becomes both
   *  the claim and its source span, so write what you want recorded rather than an
   *  instruction about it. Additive only; it is never a way to delete. */
  text?: string;
  /** verbatim_answer: how close a match must be before it can be quoted as an
   *  answer (default 0.6 server-side). Cosine scale is a property of the
   *  EMBEDDING MODEL, so tune this against your own — the response reports the
   *  actual score even when it declines to answer. */
  min_score?: number;
  /** import_md: an export_md-shaped Markdown document (headings = hierarchy;
   *  `<!-- loom: ... -->` metadata; `<!-- loom-edges: ... -->` trailer). Omit
   *  document_id to create a new document; pass it (+ optional parent_id) to
   *  import under an existing chunk. */
  markdown?: string;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.path} and
 *  {@link LoomcycleClient.document}. `unknown` because it varies per op —
 *  callers narrow as needed (e.g. an `ls` returns `{path, entries}`, a
 *  `create_document` returns `{document_id, root_chunk_id, ...}`). */
export type PathToolResponse = unknown;
export type DocumentToolResponse = unknown;

/** Input for {@link LoomcycleClient.history} — the RFC BE History tool
 *  (POST /v1/_history). Browse/search/annotate PAST CHATS (a chat = a session).
 *  Op-discriminated; the owner is resolved server-side from the authenticated
 *  principal, never the wire — you pick a `scope` selector, not an owner id.
 *  Loosely typed (the in-process tool owns the full schema); use the `[extra]`
 *  index signature for forward-compat fields. */
export type HistoryToolInput = {
  op:
    | "list"
    | "get"
    | "search"
    | "rename"
    | "annotate"
    | "pin"
    | "archive"
    | "recap"
    | "resume"
    // `related` predates this union's last update and was missing from it; `window`
    // is new. The wire passthrough carries any `op` verbatim, so the union is for
    // type-completeness rather than enforcement — which is exactly why it drifts.
    | "related"
    | "window";
  /** Whose chats: self = this caller's agent; user = this end-user's; tenant =
   *  this tenant's; global = all tenants (admin only). Default self. */
  scope?: "self" | "user" | "tenant" | "global";
  /** get/rename/annotate/pin/archive/recap/resume/window: the chat (session) id.
   *  For `window`, the session a recalled fact reported as its source. */
  session_id?: string;
  /** window: the fact's source span — the verbatim text it was distilled from,
   *  which recall returns as `source`. It anchors the window to the turn the fact
   *  came from, rather than the start of the chat. */
  quote?: string;
  /** window: how many turns either side of the match to return (default 2, max
   *  10). A distilled fact loses what its turn's neighbours carry — a bare date, a
   *  pronoun — which is what these recover. */
  context?: number;
  /** list/search: filter by derived chat status (running/completed/failed/cancelled). */
  status?: string;
  /** list/search: RFC3339 lower bound on last activity. */
  from?: string;
  /** list/search: RFC3339 upper bound on last activity. */
  to?: string;
  /** list/search: return only chats carrying this exact tag. */
  tag?: string;
  /** list: case-insensitive substring match on the title. */
  title_contains?: string;
  /** search: the text to match. What it is matched AGAINST depends on `match`. */
  query?: string;
  /** search: what to match `query` against.
   *
   *  `"title"` (the default) is the cheap path — a case-insensitive match on the
   *  chat's name, which is usually auto-generated and therefore often not what a
   *  person would search for. `"content"` searches what was actually SAID, over the
   *  turns you typed; it needs an embedder and a `user_id` on the run, and returns
   *  each chat alongside the turn that matched it in `matched_turns`. */
  match?: "title" | "content";
  /** list/search: restrict to pinned chats. */
  pinned_only?: boolean;
  /** list/search: include archived chats (excluded by default). */
  include_archived?: boolean;
  /** list/search: max chats per page (default 50, cap 500). */
  limit?: number;
  /** list/search: pagination offset. */
  offset?: number;
  /** get: "markdown" renders the transcript as Markdown instead of events. */
  format?: string;
  /** rename: the new title. */
  title?: string;
  /** annotate: the new description. */
  description?: string;
  /** annotate: the new tag set (replaces the existing set). */
  tags?: string[];
  /** pin: true pins (default), false unpins. */
  pinned?: boolean;
  /** archive: true archives (default), false unarchives. */
  archived?: boolean;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.history}. `unknown` because it
 *  varies per op — `list`/`search` return `{scope, chats, total, ...}`, `get`
 *  returns the chat metadata + transcript (or Markdown), `recap` a summary,
 *  `resume` a continuation handle. Callers narrow as needed. */
export type HistoryToolResponse = unknown;

/** Volume access mode — read-only or read-write (RFC AH). */
export type VolumeMode = "ro" | "rw";

/** One row of {@link LoomcycleClient.listVolumes} (`GET /v1/_volumes`). */
export interface PersistentVolumeEntry {
  name: string;
  /** "static" (operator yaml, read-only) or "dynamic" (a tenant VolumeDef). */
  source: "static" | "dynamic";
  /** Host path. Redacted to "" for a non-operator (tenant) caller — the
   *  volume universe is visible to a tenant operator, the host location is
   *  not. Operator-equivalent callers see the real path. */
  path: string;
  mode: VolumeMode;
  /** True for the static volume flagged `default: true` (never for dynamic). */
  default: boolean;
  /** True for the static volume dynamic VolumeDefs are provisioned inside. */
  dynamic_root: boolean;
  /** Set for dynamic rows (the substrate stamps it); absent for static. */
  created_at?: string;
}

export interface PersistentVolumesResponse {
  entries: PersistentVolumeEntry[];
}

/** One row of {@link LoomcycleClient.listEphemeralVolumes}
 *  (`GET /v1/_volumes/ephemeral`). */
export interface EphemeralVolumeEntry {
  name: string;
  root_run_id: string;
  /** Host path — redacted to "" for a non-operator caller (see
   *  {@link PersistentVolumeEntry.path}). */
  path: string;
  mode: VolumeMode;
  created_at: string;
}

export interface EphemeralVolumesResponse {
  entries: EphemeralVolumeEntry[];
}

/** Response a Post webhook returns. When result is omitted the tool
 *  result passes through unchanged. */
export interface PostHookResult {
  result?: HookToolResult;
}

// ---- v0.9.x n8n RFC Phase 0 ----

/** Aggregate stats for one operator-declared channel. Returned by
 *  {@link LoomcycleClient.listChannels}. */
export interface ChannelDescriptor {
  name: string;
  description?: string;
  scope?: string;
  semantic?: string;
  publisher?: string;
  period?: string;
  default_ttl?: number;
  max_messages?: number;
  /** Breakpoint: publishes are stored but never delivered until released. */
  hold?: boolean;
  message_count: number;
  /** RFC3339 — empty when count == 0. */
  oldest_visible_at?: string;
  newest_visible_at?: string;
  /** v0.11.5: "yaml" (operator yaml — immutable from this surface),
   *  "runtime" (substrate — CRUD-mutable), "orphan" (no declaration,
   *  only orphan messages). */
  source?: "yaml" | "runtime" | "orphan" | string;
}

/** Response shape for {@link LoomcycleClient.listChannels}. */
export interface ListChannelsResponse {
  channels: ChannelDescriptor[];
}

/** Result of {@link LoomcycleClient.purgeChannel} — the channel name and
 *  the count of buffered messages cleared. */
export interface ChannelPurgeResult {
  name: string;
  purged: number;
}

// ---- v0.9.x Channel CRUD types ----

/** Scope selector for the Channel CRUD methods. `"global"` addresses
 *  the admin surface; `"user"` requires `userId` and addresses the
 *  per-end-user URL family. */
export type ChannelScope = "global" | "user";

/** Options for {@link LoomcycleClient.publishChannel}. `payload` is
 *  the raw JSON value (object, array, string, number) to publish.
 *  `deliverAt` (RFC3339Nano) defers the publish so long-poll
 *  subscribers wake at the visible_at time. */
export interface PublishChannelOptions {
  scope: ChannelScope;
  /** Required when scope === "user". The per-user URL is
   *  /v1/users/{userId}/channels/{channel}/publish. */
  userId?: string;
  payload: unknown;
  /** RFC3339Nano deferred-publish time. Omit for "publish now". */
  deliverAt?: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.publishChannel}. */
export interface ChannelPublishResult {
  msg_id: string;
  channel: string;
  /** RFC3339Nano. */
  created_at: string;
  /** RFC3339Nano. Omitted when the publish was immediate. */
  visible_at?: string;
}

/** Options for {@link LoomcycleClient.subscribeChannel}. The call is a
 *  single-round-trip long-poll, NOT an open SSE stream — returns
 *  immediately if messages are present, otherwise waits up to
 *  `waitMs` for a publish. Auto-commits the cursor on a non-empty
 *  batch (at-most-once shape). For at-least-once, use
 *  {@link LoomcycleClient.peekChannel} + explicit ack. */
export interface SubscribeChannelOptions {
  scope: ChannelScope;
  userId?: string;
  /** Cursor to read forward from. Empty/omitted = the committed
   *  cursor. `"cur_0"` = replay from the oldest non-expired row. */
  fromCursor?: string;
  /** Defaults to 10; clamped at 100 by the server. */
  maxMessages?: number;
  /** Long-poll timeout in ms. 0 / omitted = poll once and return.
   *  Capped at the operator's `ChannelsLongPollCapMS` (default 30s). */
  waitMs?: number;
  signal?: AbortSignal;
}

/** One delivered message — same wire shape as the in-band Channel
 *  tool's subscribe response. */
export interface ChannelMessageItem {
  id: string;
  value: unknown;
  /** RFC3339Nano. */
  published_at: string;
}

/** Response shape for {@link LoomcycleClient.subscribeChannel}. */
export interface ChannelSubscribeResult {
  channel: string;
  messages: ChannelMessageItem[];
  /** Cursor to pass on the next subscribe call to continue forward.
   *  Empty when the batch is empty. */
  next_cursor: string;
}

/** Options for {@link LoomcycleClient.peekChannel}. */
export interface PeekChannelOptions {
  scope: ChannelScope;
  userId?: string;
  fromCursor?: string;
  maxMessages?: number;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.peekChannel}. */
export interface ChannelPeekResult {
  channel: string;
  messages: ChannelMessageItem[];
}

/** Options for {@link LoomcycleClient.ackChannel}. */
export interface AckChannelOptions {
  scope: ChannelScope;
  userId?: string;
  cursor: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.ackChannel}. */
export interface ChannelAckResult {
  ok: boolean;
}

// --- RFC S client twins: fan-in (await) / fan-out (broadcast) ---

/** Fan-in mode for {@link LoomcycleClient.awaitChannels}: `any` = ≥1
 *  channel has a message; `all` = every channel has ≥1; `at_least` =
 *  total messages across channels ≥ `n`. */
export type ChannelAwaitMode = "any" | "all" | "at_least";

/** Options for {@link LoomcycleClient.awaitChannels} — wait until the
 *  predicate is met across `channels`, or `waitMs` elapses. `scope` +
 *  `userId` apply to EVERY channel in the set. Non-committing. Max 32. */
export interface AwaitChannelsOptions {
  channels: string[];
  scope: ChannelScope;
  /** Required when scope === "user" — the shared scope_id for the set. */
  userId?: string;
  mode?: ChannelAwaitMode;
  /** Required (>0) when mode === "at_least". */
  n?: number;
  fromCursor?: string;
  maxMessages?: number;
  /** Long-poll timeout in ms; capped at the operator's LongPollCapMS. */
  waitMs?: number;
  signal?: AbortSignal;
}

/** One fired channel's accumulated messages + the (non-advanced) cursor. */
export interface ChannelAwaitEntry {
  messages: ChannelMessageItem[];
  next_cursor: string;
}

/** Response shape for {@link LoomcycleClient.awaitChannels}. `timed_out`
 *  is true only when the predicate was unmet within `waitMs` (never an
 *  error). `results` is keyed by channel name. */
export interface ChannelAwaitResult {
  satisfied: boolean;
  timed_out: boolean;
  mode: ChannelAwaitMode;
  fired: string[];
  total_messages: number;
  results: Record<string, ChannelAwaitEntry>;
}

/** Options for {@link LoomcycleClient.broadcastChannels} — publish the
 *  same `payload` to every channel in `channels`. Atomic at the declare
 *  pre-flight (one undeclared channel rejects the whole call). Max 32. */
export interface BroadcastChannelsOptions {
  channels: string[];
  scope: ChannelScope;
  userId?: string;
  payload: unknown;
  /** RFC3339Nano deferred-publish time. Omit for "publish now". */
  deliverAt?: string;
  signal?: AbortSignal;
}

/** One channel's publish outcome. `error` is set (and `msg_id` absent)
 *  when that channel's write failed after the pre-flight passed. */
export interface ChannelBroadcastEntry {
  channel: string;
  msg_id?: string;
  created_at?: string;
  visible_at?: string;
  error?: string;
}

/** Response shape for {@link LoomcycleClient.broadcastChannels}.
 *  `published` + `failed` = the deduped channel count. */
export interface ChannelBroadcastResult {
  published: number;
  failed: number;
  results: ChannelBroadcastEntry[];
}

// ---- v0.11.5 Channel admin CRUD types ----

/** Options for {@link LoomcycleClient.createChannel}. Operator-yaml
 *  channels are immutable from this surface; the server returns
 *  HTTP 409 `channel_yaml_immutable` when `name` matches a yaml-
 *  declared channel. */
export interface CreateChannelOptions {
  name: string;
  description?: string;
  /** "global" | "agent" | "user". Defaults to "global" if omitted. */
  scope?: string;
  /** "queue" | "topic". Defaults to "queue" if omitted. */
  semantic?: string;
  /** Seconds; 0 = no TTL. */
  default_ttl?: number;
  /** 0 = unbounded. */
  max_messages?: number;
  /** Free-form attribution; not enforced by the substrate. */
  publisher?: string;
  /** Free-form retention hint; not enforced by the substrate. */
  period?: string;
  /** Breakpoint: publishes are stored but never delivered until
   *  {@link LoomcycleClient.releaseChannel} hands them over. */
  hold?: boolean;
  signal?: AbortSignal;
}

/** Options for {@link LoomcycleClient.updateChannel}. Nil fields
 *  leave the corresponding channel attribute unchanged. */
export interface UpdateChannelOptions {
  description?: string;
  default_ttl?: number;
  max_messages?: number;
  /** "queue" | "topic" */
  semantic?: string;
  /** Turn the breakpoint on or off. Messages already held stay held
   *  until released — turning it off does not flush the queue. */
  hold?: boolean;
  signal?: AbortSignal;
}

/** Options for {@link LoomcycleClient.releaseChannel}. */
export interface ReleaseChannelOptions {
  /** How many held messages to hand over, oldest first. Default 1. */
  count?: number;
  /** "global" (default) | "user" | "tenant". */
  scope?: string;
  /** Required when scope is "user". */
  scope_id?: string;
  signal?: AbortSignal;
}

/** Result of {@link LoomcycleClient.releaseChannel}. `released` is the
 *  ids handed over, in delivery order; `still_held` is what is left. */
export interface ChannelReleaseResult {
  channel: string;
  released: string[];
  released_count: number;
  still_held: number;
}

// ---- v0.11.5 Memory entry admin CRUD types ----

/** Options for {@link LoomcycleClient.setMemoryEntry}. `value` is
 *  opaque JSON. Setting `embed: true` triggers a synchronous embed
 *  via the operator-configured embedder; the returned `embedded`
 *  flag + optional `embed_warning` report whether the embedding
 *  landed. */
export interface SetMemoryEntryOptions {
  value: unknown;
  /** When true, also compute + store the embedding (requires
   *  memory.embedder yaml + a vector-capable store backend). */
  embed?: boolean;
  /** Optional TTL in seconds; <= 0 means "no expiry". */
  ttl_seconds?: number;
  /** Super-admin tenant focus. Ignored server-side for a tenant-scoped
   *  principal, so it can never widen a caller's own scope; omitted, the write
   *  lands in the caller's own tenant. */
  tenant?: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.setMemoryEntry}. */
export interface SetMemoryEntryResponse {
  scope: string;
  scope_id: string;
  key: string;
  /** true when the embedding was computed AND stored. */
  embedded: boolean;
  /** Non-empty when embed was requested but failed (transient
   *  error, embedder unconfigured, vector backend not available).
   *  The k/v row still landed. */
  embed_warning?: string;
}

/** One run state transition emitted by
 *  {@link LoomcycleClient.streamUserRunStates}. The TS field is RFC3339. */
export interface RunStateEvent {
  run_id: string;
  agent_id: string;
  agent: string;
  user_id: string;
  parent_agent_id?: string;
  status: string;
  stop_reason?: string;
  error?: string;
  ts: string;
  /** Opaque caller-tracking lineage echoed on the transition (v0.12.x),
   *  so a subscriber learns which root request a finishing sub-agent
   *  belongs to. Omitted when the run carried no context. */
  parent_context?: ParentContext;
}

/** Initial stream_open frame emitted before the first run_state. */
export interface RunStateStreamOpen {
  user_id: string;
  filter_status: string[] | null;
  filter_agent: string;
  /** v1.78.0 — the walk filter the server actually applied, echoed back.
   *
   *  Worth reading rather than assuming: a filter that matched nothing and a
   *  filter the server never understood look identical from the outside — both
   *  are a stream that stays quiet. Comparing this to what you sent tells the
   *  two apart before you wait on an empty stream. Empty when unfiltered. */
  filter_walk_id?: string;
  keepalive_interval: number;
}

/** Yielded by {@link LoomcycleClient.streamUserRunStates}.
 *
 *  The first item is always `{ kind: "open", payload: RunStateStreamOpen }`.
 *  Subsequent items are `{ kind: "event", payload: RunStateEvent }`.
 *
 *  Consumers branch on `kind`; the `open` frame is useful for confirming
 *  the connection before any real events flow. */
export type RunStateStreamItem =
  | { kind: "open"; payload: RunStateStreamOpen }
  | { kind: "event"; payload: RunStateEvent }
  // v0.9.x — emitted ONLY when StreamUserRunStatesOptions.debug=true.
  // Never on the wire; synthesized client-side at stream-close time.
  | { kind: "close"; payload: RunStateStreamClose };

/** Optional filter for {@link LoomcycleClient.streamUserRunStates}. */
export interface StreamUserRunStatesOptions {
  /** Subset of states to receive. Empty means all states. */
  statuses?: string[];
  /** Filter to one agent name. Empty means any. */
  agent?: string;
  /** v1.78.0 — SERVER-side filter: only the runs one team walk spawned,
   *  matched against each event's `parent_context.walk_id`.
   *
   *  A team walk's own run_id IS its walk id, so a caller that started a team
   *  with `detach: true` passes back exactly the handle it already holds —
   *  there is no second identifier and nothing to map. That is what makes a
   *  live view of one running workflow's agents a filter rather than a
   *  feature.
   *
   *  Unlike {@link StreamUserRunStatesOptions.parentAgentId}, this is applied
   *  by the server, so it reduces what crosses the wire and not just what your
   *  callback sees. The server echoes it back on the open frame as
   *  {@link RunStateStreamOpen.filter_walk_id}. */
  walkId?: string;
  /** v0.9.x — client-side filter on the run's parent_agent_id.
   *  Useful for "show me only the sub-runs spawned by agent X."
   *  The filter is applied AFTER the SSE frame is parsed, so this
   *  shrinks what your callback sees but doesn't reduce server-side
   *  load. Pass the empty string to opt out (default).
   *
   *  For the one case that HAS a server-side filter, prefer it: see
   *  {@link StreamUserRunStatesOptions.walkId}. */
  parentAgentId?: string;
  /** v0.9.x — opt-in observability: when true, the iterator yields a
   *  client-synthesized `{ kind: "close", payload: { reason } }` item
   *  when the stream ends (EOF, abort, or error). `reason` carries
   *  the cause ("eof" on clean close or an error class name like
   *  "AbortError" / "AuthError"). The opening `kind: "open"` frame
   *  that always appears first is server-emitted, not synthetic;
   *  `debug` has no effect on it. Default false leaves behaviour
   *  identical to v0.9.x earlier. */
  debug?: boolean;
  signal?: AbortSignal;
}

/** v0.9.x — close-event payload emitted only under
 *  {@link StreamUserRunStatesOptions.debug}. Synthetic; never on the
 *  wire. */
export interface RunStateStreamClose {
  reason: string;
}

// ---- v0.9.x content_sha256 verify op (AgentDef + SkillDef) ----

/** Response shape for `AgentDef set/fork/get/list` rows. Mirrors what
 *  the server-side rowResponseMap emits. The `content_sha256` field is
 *  the deterministic SHA-256 of the agent's content-bearing fields,
 *  prefixed `sha256:` (Docker image-digest convention). Empty on rows
 *  that pre-date v0.9.x and haven't been backfilled yet. */
export interface AgentDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  /** "sha256:" + 64 hex chars; empty for not-yet-backfilled rows. */
  content_sha256?: string;
  /** Only populated on `set` / `fork` responses (was the new row
   *  auto-promoted to active?). Absent on get/list. */
  promoted?: boolean;
  /** True when create was a content-addressed no-op: the active def already
   *  carried identical content, so no new version was minted. Absent on a
   *  real mint. Drives {@link LoomcycleClient.ensureCodeAgent}'s `changed`. */
  deduplicated?: boolean;
}

/** Response shape for `AgentDef verify`. Answers "is the supplied
 *  content_sha256 the active deployed version of this name?"
 *
 *  - `matches: true`  — caller's local hash matches the deployed
 *                       active version; no push needed.
 *  - `matches: false` — bundle is out of sync; the operator should
 *                       push a new version via `agentDef({op: "set",
 *                       overlay: ...})`.
 *  - `deployed: false` — no active row exists for this name (no
 *                       deployment yet). matches is always false. */
export interface AgentDefVerifyResult {
  matches: boolean;
  /** Deployed active row's hash; empty when not deployed. */
  current_sha256: string;
  /** Deployed active row's def_id; empty when not deployed. */
  current_def_id: string;
  /** Deployed active row's version; 0 when not deployed. */
  version: number;
  name: string;
  /** True if an active row exists for this name. */
  deployed: boolean;
}

/** Response shape for `SkillDef verify`. Same semantics as
 *  AgentDefVerifyResult; the per-skill content basis is just
 *  smaller (name + description + body + tools). */
export interface SkillDefVerifyResult {
  matches: boolean;
  current_sha256: string;
  current_def_id: string;
  version: number;
  name: string;
  deployed: boolean;
}

// ---- v0.9.x MCPServerDef substrate (dynamic MCP server registration) ----

/** Response shape for `MCPServerDef set/fork/get/list` rows. Mirrors
 *  what the server-side rowResponseMap emits. `discovered_tools` is the
 *  cached tools/list snapshot — refreshed via the `rediscover` op; not
 *  part of the content_sha256 basis. */
export interface MCPServerDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  /** "sha256:" + 64 hex chars; empty for not-yet-backfilled rows. */
  content_sha256?: string;
  /** Only populated on `set` / `fork` responses (auto-promoted?). */
  promoted?: boolean;
  /** True when create/rediscover was a content-addressed no-op
   *  (loomcycle ≥ v0.18.0): the active def already carried identical
   *  content (create) or identical discovered_tools (rediscover), so no
   *  new version was minted. Absent (undefined) on a real mint. */
  deduplicated?: boolean;
  /** Number of tools discovered via the upstream's `tools/list`. Present on
   *  `rediscover` responses and — since loomcycle auto-discovers at ingestion
   *  — on a fresh `create`/`fork` that ran the handshake. Absent on a
   *  deduplicated create (the active tool surface was unchanged) and when
   *  `discover:false` was passed. */
  discovered?: number;
}

/** Options for {@link LoomcycleClient.ensureMcpServer}. */
export interface EnsureMcpServerOptions {
  /** Substrate name (not a static cfg.MCPServers name). */
  name: string;
  /** Absolute MCP endpoint, e.g. `http://localhost:3000/api/mcp`. */
  url: string;
  /** Default `"http"`. */
  transport?: "http" | "streamable-http";
  /** Per-request headers, stored verbatim. Keep `${run.*}` / `${LOOMCYCLE_*}`
   *  substitution placeholders LITERAL (don't resolve a token yourself) so
   *  the registration content is stable across restarts — that's what lets
   *  loomcycle's idempotent create dedup the re-registration. */
  headers?: Record<string, string>;
  description?: string;
  /** Force a `tools/list` refresh after registering. Default false.
   *
   *  You usually do NOT need this: registration auto-discovers the upstream's
   *  tools at ingestion, so {@link EnsureMcpServerResult.discoveredToolCount}
   *  is already populated from the `create`. Set this only to force a re-read
   *  when the upstream's tool surface changed but the registration content
   *  (url/headers) did not — a plain re-register would dedup and keep the
   *  cached tools, whereas a rediscover re-runs the handshake. */
  rediscover?: boolean;
}

/** Result of {@link LoomcycleClient.ensureMcpServer}. */
export interface EnsureMcpServerResult {
  name: string;
  defId: string;
  version: number;
  /** True when this call minted a new version (create and/or rediscover);
   *  false when loomcycle deduped it (active def already current). A
   *  consumer re-registering on every boot expects `changed: false` once
   *  the registration content is stable. */
  changed: boolean;
  /** Populated when `rediscover` ran: the number of tools discovered. */
  discoveredToolCount?: number;
}

/** Typed `overlay` for an {@link LoomcycleClient.agentDef} create/fork —
 *  the mutable subset of an agent definition. All fields optional (a fork
 *  overlays only what changes). The `[extra]` tail keeps it forward-compatible
 *  with fields the in-process tool may accept that the adapter doesn't model
 *  yet — the tool owns the authoritative schema; the adapter doesn't re-validate.
 *
 *  `code_body` is the inline code-js orchestrator source (RFC J): set it (with
 *  `provider: "code-js"`) to ingest a code agent through the substrate with NO
 *  host filesystem bind — the symmetry that makes code agents work in
 *  containers / pure-cloud. Requires `LOOMCYCLE_CODE_AGENTS_ENABLED=1` on the
 *  sidecar; create/fork refuses a non-empty `code_body` otherwise. */
/** Per-agent Channel tool ACL (mirrors the sidecar `channels:` agent yaml).
 *  The Channel tool default-denies until publish/subscribe patterns are granted. */
export interface AgentChannelACL {
  publish?: string[];
  subscribe?: string[];
}

/** Per-agent Interruption tool gate (mirrors the sidecar `interruption:` agent
 *  yaml). `enabled: true` is REQUIRED for the Interruption tool to work at all. */
export interface AgentInterruptionACL {
  enabled?: boolean;
  kinds?: string[];
  max_pending?: number;
}

export interface AgentDefOverlay {
  provider?: string;
  model?: string;
  /** Inline code-js source. Empty/absent ⇒ the provider falls back to
   *  `agent_code/<name>/index.js`. Stored verbatim (whitespace is hash-
   *  significant); participates in content_sha256. */
  code_body?: string;
  tier?: string;
  effort?: string;
  /** Whether and which tool the model must call (RFC DI). */
  tool_choice?: ToolChoiceOptions;
  max_tokens?: number;
  max_iterations?: number;
  max_concurrent_children?: number;
  system_prompt?: string;
  tools?: string[];
  skills?: string[];
  memory_scopes?: string[];
  /** RFC DF: recall attaches the conversation TURN each fact was distilled from,
   *  for this agent, WITHOUT the model asking for it. Operator-set on purpose — a
   *  tool parameter is a decision the model makes, and measured across three local
   *  models they do not make it. Also gated by `history_scope`: this decides whether
   *  turns are OFFERED, history_scope whether they may be READ. */
  recall_include_turns?: boolean;
  /** Also run the QUESTION-anchored trace search on every `recall` and return those
   *  turns as their own block, beside the facts.
   *
   *  Distinct from `recall_include_turns`, which attaches the turn each recalled FACT
   *  was distilled from: fact-anchored retrieval can only reach turns some fact was
   *  already extracted from, and the turns that answer the rest are the ones the
   *  extractor passed over. Measured on one corpus the two routes differ by 24 points.
   *
   *  Operator-set, with deliberately NO tool parameter: told to pass the sibling's
   *  parameter on every call, one model passed it on 51 of 128. Also gated by
   *  `history_scope`, and needs the trace index enabled AND backfilled — an empty
   *  index yields zero turns silently. */
  recall_attach_traces?: boolean;
  memory_quota_bytes?: number;
  memory_backend?: string;
  retry_attempts?: number;
  /** Evaluation tool scope gate, e.g. `["submit_self", "read_any"]`. The
   *  Evaluation tool default-denies until granted. */
  evaluation_scopes?: string[];
  /** Channel tool ACL (default-deny until set). */
  channels?: AgentChannelACL;
  /** Interruption tool gate — `enabled: true` REQUIRED for the tool to work. */
  interruption?: AgentInterruptionACL;
  [extra: string]: unknown;
}

/** Options for {@link LoomcycleClient.ensureCodeAgent}. */
export interface EnsureCodeAgentOptions {
  /** Agent name (substrate; must not collide with a static cfg.Agents name —
   *  use a fork for those). */
  name: string;
  /** The inline code-js orchestrator source (the `function run(input){…}` body).
   *  Keep any `${run.*}` / `${LOOMCYCLE_*}` placeholders LITERAL so the content
   *  is stable across restarts — that's what lets loomcycle dedup the
   *  re-registration. */
  code: string;
  /** The agent's tools ceiling (must be a subset of the caller's). */
  tools?: string[];
  /** Per-user tier policy name (mutually exclusive with `model` in practice). */
  tier?: string;
  /** Pin a concrete model id (overrides tier resolution). */
  model?: string;
  description?: string;
}

/** Result of {@link LoomcycleClient.ensureCodeAgent}. */
export interface EnsureCodeAgentResult {
  name: string;
  defId: string;
  version: number;
  /** True when this call minted a new version; false when loomcycle deduped
   *  it (identical body + config already active). */
  changed: boolean;
}

/** Response shape for `MCPServerDef verify`. Same semantics as
 *  AgentDefVerifyResult / SkillDefVerifyResult — answers "is the
 *  supplied content_sha256 the deployed active version of this name?" */
export interface MCPServerDefVerifyResult {
  matches: boolean;
  /** Deployed active row's hash; empty when not deployed. */
  current_sha256: string;
  /** Deployed active row's def_id; empty when not deployed. */
  current_def_id: string;
  /** Deployed active row's version; 0 when not deployed. */
  version: number;
  name: string;
  /** True if an active row exists for this name. */
  deployed: boolean;
}

// ---- v0.10.3 Library v2 — unified yaml + substrate enumeration ----
//
// Wraps GET /v1/_library/{agents,skills,mcp-servers} (shipped in v0.9.3).
// Each endpoint merges yaml-static entries with substrate-side dynamic
// entries into one envelope per row, tagged with its source. Adapter
// methods are listLibraryAgents / listLibrarySkills / listLibraryMcpServers
// — typed wrappers around the existing endpoints so external consumers
// (n8n, custom integrations) don't need to drop to raw fetch + path
// strings.
//
// The TypeScript shape mirrors web/src/api.ts:825-844 (the Web UI's
// canonical definitions) plus tightened static_definition typing per
// endpoint flavor — Web UI uses `unknown` because it renders many
// shapes through one component; external adapters get IntelliSense
// over the per-flavor structural type.

/** Static-side agent definition body. Snake_case keys mirror
 *  internal/lookup.SubstrateAgentDef so substrate-side and static-side
 *  definitions share one renderer. Forward-compatible: unknown fields
 *  on newer binaries flow through transparently. */
export interface LibraryAgentDefinition {
  provider?: string;
  model?: string;
  tier?: string;
  effort?: string;
  max_tokens?: number;
  max_iterations?: number;
  system_prompt?: string;
  system_prompt_base?: string;
  tools?: string[];
  skills?: string[];
  providers?: string[];
  /** RFC BB: per-agent web-search fallback list — the ordered providers the
   *  WebSearch tool tries (empty = the global search_priority default). */
  search_providers?: string[];
  /** Per-tier candidate list. Server-side opaque shape — kept as
   *  Record<string, unknown> for forward-compat. */
  models?: Record<string, unknown>;
  memory_scopes?: string[];
  memory_quota_bytes?: number;
}

/** Static-side skill definition body. */
export interface LibrarySkillDefinition {
  body?: string;
  description?: string;
  tools?: string[];
}

/** Static-side MCP server definition body. Mirrors
 *  internal/api/http.marshalStaticMCPServer (transport + url + headers
 *  for http/streamable-http; command/args/env/pool_size for stdio;
 *  tools narrowing; discovered_tools cached from the pool
 *  inspector when ready). */
export interface LibraryMcpServerDefinition {
  transport?: "http" | "streamable-http" | "stdio";
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  pool_size?: number;
  tools?: string[];
  /** Substrate-mirror shape of the pool's PeekTools snapshot.
   *  Omitted when the pool inspector returns nil (init pending or
   *  failed) — re-check after pool init completes. */
  discovered_tools?: unknown;
}

/** One row of the library envelope. T is the per-endpoint definition
 *  type — LibraryAgentDefinition for /v1/_library/agents, etc. */
export interface LibraryEntry<T = unknown> {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  /** Static-side definition body. Omitted when in_static is false
   *  (dynamic-only entries have no static body — query the substrate
   *  via AgentDef/SkillDef/MCPServerDef tools to fetch the active
   *  version's payload). */
  static_definition?: T;
}

/** Envelope returned by all three GET /v1/_library/* endpoints. */
export interface LibraryListResponse<T = unknown> {
  entries: LibraryEntry<T>[];
}

// ---- v0.11.0 LLM Gateway (`POST /v1/_llm/chat`) ----
//
// Direct LLM call surface that bypasses the agent loop. Consumers
// (n8n's LoomCycleChatModel cluster sub-node first; any LangChain-
// compatible adapter in principle) hit the gateway when they only
// need provider routing + auth + retry — no tools, no memory, no
// agent semantics. The on-the-wire shape is LangChain-friendly on
// the request side (flat content strings + tool_call_id correlation)
// and Anthropic-style on the response side (content-block arrays
// with type-discriminated unions).

/** One message in the gateway conversation. Mirrors LangChain's
 *  BaseMessage shape so consumers map without re-shaping. */
export interface LLMChatMessage {
  role: "system" | "user" | "assistant" | "tool";
  /** Flat string content. For "assistant" turns with tool_calls,
   *  the content may be empty. For "tool" turns, this is the tool
   *  result text. */
  content?: string;
  /** Set on "assistant" turns that requested tool invocations. */
  tool_calls?: LLMChatToolCall[];
  /** Set on "tool" turns; correlates back to the assistant's
   *  tool_calls[].id. */
  tool_call_id?: string;
}

/** One assistant-requested tool invocation. */
export interface LLMChatToolCall {
  id: string;
  name: string;
  input: Record<string, unknown>;
}

/** Tool the model may call. The substrate translates this into the
 *  driver-native shape (Anthropic input_schema vs OpenAI function.
 *  parameters vs Gemini function_declarations) — caller passes the
 *  flat JSON schema and trusts the gateway's per-driver translation. */
export interface LLMTool {
  name: string;
  description?: string;
  input_schema: Record<string, unknown>;
}

/** Request body for llmChat / llmStream.
 *
 *  Two RFC-mentioned fields are deliberately absent in v1:
 *  - `stop_sequences`: providers.Request has no matching field today;
 *    accepting it would silently drop it. Lands when the providers
 *    package surface grows the equivalent.
 *  - `user_bearer`: the gateway calls provider.Call() directly with
 *    no MCP transport, so `${run.user_bearer}` substitution has
 *    nowhere to apply. Lands when the gateway grows an MCP path. */
export interface LLMChatOptions {
  messages: LLMChatMessage[];
  tools?: LLMTool[];
  max_tokens?: number;
  temperature?: number | null;

  /** Routing hint. When set with `model`, the resolver short-circuits
   *  to that explicit pin. When set alone, the resolver picks the
   *  best model in that provider given tier/user_tier. */
  provider?: string;
  /** Routing hint. When set with `provider`, explicit pin. When set
   *  alone, the resolver picks the provider hosting that model. */
  model?: string;
  /** Tier for resolver dispatch. Defaults to "default" when neither
   *  pin nor tier supplied. */
  tier?: string;

  /** Per-user quota tracking. Empty bypasses the per-user cap. */
  user_id?: string;
  /** Per-user tier overlay; takes precedence over `tier` when set. */
  user_tier?: string;

  /** Optional AbortSignal for caller-driven cancellation. */
  signal?: AbortSignal;
}

/** Non-streaming response shape. */
export interface LLMChatResponse {
  /** Per-response id (llm_<hex>); useful in audit logs. */
  id: string;
  /** Per-request id (req_<hex>); cross-references the audit log. */
  request_id: string;
  /** Which provider the resolver picked. */
  provider: string;
  /** Specific model id picked. */
  model: string;
  /** Content blocks; one per text or tool_use output. */
  content: LLMChatContent[];
  stop_reason: "end_turn" | "max_tokens" | "tool_use" | "stop_sequence";
  usage: LLMChatUsage;
}

/** One output content block. */
export type LLMChatContent =
  | { type: "text"; text: string }
  | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };

/** Token-accounting payload. Cache fields are populated only on
 *  providers that surface them (Anthropic today). */
export interface LLMChatUsage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
}

/** One streaming-mode SSE frame. The `kind` field is the SSE event
 *  name; the `payload` carries the per-frame shape. v1 mirrors
 *  Anthropic's streaming event names. */
export type LLMChatStreamItem =
  | { kind: "provider_chosen"; payload: { provider: string; model: string; request_id: string } }
  | { kind: "content_block_start"; payload: { index: number; block: LLMChatContent } }
  | { kind: "content_block_delta"; payload: { index: number; delta: LLMChatStreamDelta } }
  | { kind: "content_block_stop"; payload: { index: number } }
  | { kind: "message_delta"; payload: { delta: { stop_reason?: string }; usage: LLMChatUsage } }
  | { kind: "done"; payload: { id: string; stop_reason: string; usage: LLMChatUsage } }
  | { kind: "error"; payload: { type: string; code: string; message: string } };

export interface LLMChatStreamDelta {
  type: "text_delta" | "input_json_delta";
  text?: string;          // for text_delta
  partial_json?: string;  // for input_json_delta
}

// ---- v0.11.4 OpenAI Embeddings compatibility shim ----
//
// Wraps POST /v1/embeddings. Dispatches to the single configured
// `providers.Embedder` (the same instance Memory tool uses for
// embed:true). No resolver path, no tier overlay, no streaming.
//
// Consumers using @loomcycle/client get richer typing here than
// pointing the raw OpenAI SDK at /v1/embeddings — the response
// shape is properly typed per encoding_format.

/** Request body for `embeddings()`. */
export interface LLMEmbeddingsOptions {
  /** Consumer's requested model id (echoed in the response).
   *  Loomcycle uses the single configured embedder regardless;
   *  the field is informational for drop-in compatibility. */
  model: string;

  /** Text(s) to embed. v1 accepts string or string[]; tokenized
   *  inputs (number arrays) are refused — send text. */
  input: string | string[];

  /** "float" (default) emits each vector as a JSON array of
   *  numbers; "base64" packs each float32 little-endian then
   *  base64-encodes (saves ~25% wire bytes on 1536-dim vectors). */
  encoding_format?: "float" | "base64";

  /** OpenAI's post-hoc dimension reduction parameter. Accepted-
   *  but-ignored in v0.11.4 (the providers.Embedder interface
   *  doesn't take a dimension parameter today). */
  dimensions?: number;

  /** OpenAI-standard opaque end-user identifier. Maps to
   *  loomcycle's per-user quota tracking + audit log. */
  user?: string;

  /** Optional AbortSignal for caller-driven cancellation. */
  signal?: AbortSignal;
}

/** Per-embedding entry in the response data array. */
export interface LLMEmbeddingItem {
  object: "embedding";
  /** float[] when the request used encoding_format:"float"
   *  (default); base64 string when encoding_format:"base64". */
  embedding: number[] | string;
  index: number;
}

/** Token-accounting payload. v0.11.4 leaves both fields at 0 — the
 *  substrate's Embedder interface doesn't return per-call token
 *  counts today. When that lands, the shim's translator populates
 *  these automatically. */
export interface LLMEmbeddingsUsage {
  prompt_tokens: number;
  total_tokens: number;
}

/** Response shape mirrors OpenAI's /v1/embeddings exactly. */
export interface LLMEmbeddingsResponse {
  object: "list";
  data: LLMEmbeddingItem[];
  model: string;
  usage: LLMEmbeddingsUsage;
}

// --- RFC AV: token-usage & cost report (GET /v1/_usage) ---

/** A whitelisted grouping dimension for the usage report. */
export type UsageDimension =
  | "tenant"
  | "user"
  | "provider"
  | "model"
  | "source";

/** One grouped row of a usage report; only the grouped dimensions are set. */
export interface UsageAggregate {
  tenant_id?: string;
  user_id?: string;
  provider?: string;
  model?: string;
  /** operator | tenant | user */
  credential_source?: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  cost: number;
  currency?: string;
  call_count: number;
  unpriced_calls: number;
}

export interface UsageReportResponse {
  group_by: string[];
  from?: string;
  to?: string;
  rows: UsageAggregate[];
}

/** A per-scope token budget (RFC AW) plus its live month-to-date usage.
 *  `soft_limit` / `hard_limit` are absent when that tier is unset (no ceiling
 *  on that axis). Mirrors one row of GET /v1/_limits. */
export interface TokenLimit {
  tenant_id: string;
  /** "operator" | "tenant" | "user" */
  scope: string;
  /** tenant id (scope=tenant), user subject (scope=user), "" (operator). */
  scope_id?: string;
  soft_limit?: number;
  hard_limit?: number;
  /** The scope's current month-to-date token total. */
  used: number;
  updated_at?: string;
  updated_by?: string;
}

export interface TokenLimitsResponse {
  limits: TokenLimit[];
}

/** One LLM provider the deployment can route to. `active` is reachable AND not
 *  excluded — one coarse boolean. A provider probe error is never exposed here
 *  (those can carry hostnames, ports, and upstream response bodies); an operator
 *  gets that detail from `GET /v1/_routing`. */
export interface ConfigProvider {
  provider: string;
  active: boolean;
}

/** One (provider, model) pair the deployment can route to, flattened and
 *  deduplicated across plans — the same model appears in every plan that
 *  includes it, and a consumer wants it once. */
export interface ConfigModel {
  provider: string;
  model: string;
  /** The canonical capability tiers (low/middle/high) this pair serves. These
   *  are loomcycle's own tier names, never the operator's plan names. */
  tiers: string[];
  /** At least one tier would route here right now. */
  active: boolean;
  /** It is the FIRST available candidate for at least one tier — what runs. */
  selected: boolean;
}

/** One web-search provider (RFC BB), in cascade order. */
export interface ConfigSearch {
  provider: string;
  active: boolean;
  primary: boolean;
}

export interface ConfigInstance {
  /** Identifies the software, not the deployment, so it is public. */
  version?: string;
  /** Build provenance — absent from the `public` view. */
  commit?: string;
  build_time?: string;
  /** The operator's advertised base URL — absent from the `public` view. */
  url?: string;
}

/** `GET /v1/config` — what this instance is and what it can do.
 *
 *  Rendered at one of three disclosure levels, named in `view` so a consumer can
 *  tell "this deployment has none" from "you weren't shown them":
 *
 *  - `public` — version, features, providers, models, search. Reachable with NO
 *    bearer, and only on a deployment running `LOOMCYCLE_PUBLIC_CONFIG=1`.
 *  - `authenticated` — adds commit, build_time, url, limits, user_tiers.
 *  - `admin` — adds `features.storage`.
 *
 *  In the `public` view every `features` value is a plain boolean; at the other
 *  levels each is an object carrying at least `available`. That is deliberate:
 *  the public view is built by copying only the `available` field, so a
 *  capability gaining a new field later cannot leak to a public reader. */
export interface ConfigResponse {
  generated_at: string;
  view: "public" | "authenticated" | "admin";
  instance: ConfigInstance;
  /** Per-subsystem availability. A `boolean` in the `public` view; otherwise an
   *  object with `available` plus that capability's own detail. */
  features: Record<string, boolean | Record<string, unknown>>;
  providers: ConfigProvider[];
  models: ConfigModel[];
  search: ConfigSearch[];
  /** Configured plan names — omitted from the `public` view. */
  user_tiers?: string[];
  /** Deployment caps — omitted from the `public` view. */
  limits?: Record<string, number>;
}

/** The PUT /v1/_limits body (RFC AW). A present `soft_limit`/`hard_limit` sets
 *  that tier; omitting it clears the tier (unlimited on that axis) — a full-row
 *  upsert. `tenant_id` is an admin-only target; a tenant operator is confined to
 *  its own tenant regardless of this field. */
export interface SetTokenLimitRequest {
  /** Admin-only target tenant; ignored for confinement on a scoped caller. */
  tenant_id?: string;
  /** "operator" | "tenant" | "user" */
  scope: string;
  /** Required for scope=user (the subject); must be empty for scope=tenant. */
  scope_id?: string;
  soft_limit?: number;
  hard_limit?: number;
}

// MemoryBackfillResponse — POST /v1/_memory/backfill_embeddings.
//
// `skipped_empty` is reported rather than folded into a failure count because those rows
// have NO text to embed (a document root, a section heading) and therefore remain
// candidates permanently. Without it an operator watches `candidates` stop falling with
// `embedded` at 0 and no stated reason.
export interface MemoryBackfillResponse {
  scope: string;
  scope_id: string;
  prefix?: string;
  dry_run: boolean;
  /** Unembedded rows this call SAW, bounded by limit. */
  candidates: number;
  embedded?: number;
  failed?: number;
  skipped_empty?: number;
  /** The limit was reached with candidates outstanding — another call has work. */
  more?: boolean;
  failed_keys?: string[];
  sample_keys?: string[];
  notes?: string[];
}

// MemoryPurgeResponse — POST /v1/_memory/purge_stale_embeddings.
//
// `truncated` means the scan stopped at the limit, so a zero `stale` does NOT mean the
// scope is clean — a distinction worth keeping because this op deletes.
export interface MemoryPurgeResponse {
  scope: string;
  scope_id: string;
  prefix?: string;
  dry_run: boolean;
  scanned: number;
  /** Rows with no indexable text that nonetheless carry an embedding. */
  stale: number;
  purged: number;
  failed?: number;
  truncated: boolean;
  sample_keys?: string[];
  failed_keys?: string[];
  first_failure?: string;
  notes?: string[];
}

// ─── RFC AR credential store (RFC CN adapter parity) ─────────────────────────

/** CredentialScope buckets a stored secret. `tenant` = shared across the tenant
 *  (operator authority); `user` = the calling principal's own subject (per-user
 *  tokens). RFC CN: a user token may only address `user`; `tenant`/`agent`
 *  require substrate:tenant. scope_id is derived server-side, never sent. */
export type CredentialScope = "tenant" | "user";

/** CredentialMeta is one credential's METADATA — never the secret value (the API
 *  returns none for list/get). */
export interface CredentialMeta {
  name: string;
  scope: string;
  backend?: string;
  created_at?: string;
  updated_at?: string;
  expires_at?: string;
  status?: string;
}

/** CredentialListResponse is the {op:list} result: metadata for the given scope. */
export interface CredentialListResponse {
  scope: string;
  credentials: CredentialMeta[];
}
