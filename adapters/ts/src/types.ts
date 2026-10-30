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
  // RFC DJ: a run armed for review finished its answer and is held for an
  // operator's verdict ({@link LoomcycleClient.reviewRun}).
  | "awaiting_review"
  // On a parent's stream: a sub-agent it started through the Agent tool is
  // held for a verdict ("held") or has ended ("released").
  | "subagent_hold"
  // A run ended its turn with background (poll-mode) children still running
  // and waits for all of them; `children_note` is the runtime note its next
  // model call carries about children that ended.
  | "awaiting_children"
  | "children_note"
  | "steer"
  | "context_compaction"
  // RFC AW per-scope token budgets. `limit` = a server-generated token-budget
  // crossing (a soft warning at run start, or a soft crossing mid-run). The
  // structured payload rides `AgentEvent.limit`.
  | "capability_inert"
  // What a tool-use hook did to a call: deny, rewrite_input, rewrite_output,
  // context, or unavailable. Nothing is emitted for a call a hook passed.
  | "hook_decision"
  | "limit"
  // A file a tool returned was kept in the run tree's inbox volume instead of
  // reaching the model as bytes; the model saw a one-line stub. The payload
  // rides `AgentEvent.file`.
  | "file_captured"
  // One OCR or speech-to-text pass a File tool call made over an inbox file:
  // the source, the engine, the units and tokens spent and the cost, never the
  // text. The payload rides `AgentEvent.transcription`.
  | "transcription"
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
  // On a team walk's own run: one visit to a `decision` state, with the
  // transition taken and the model's answer (`AgentEvent.team_decision`).
  | "team_decision"
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
  // in-flight child durable across a pause. spawn_child_read records that a
  // run was first handed a poll-mode child's result. Transcript records, never
  // sent on the live stream.
  | "spawn_child_started"
  | "spawn_child_result"
  | "spawn_child_read"
  // The prompt a run's first model call received (RFC DI). A transcript
  // record read via getRunPrompt; never sent on the live stream.
  | "prompt_snapshot"
  // Run admission: `queued` = the run was accepted but waits for a run slot
  // (its user already has its cap of runs active, or every slot is taken);
  // `admitted` = it got its slot and is starting. Server-generated before the
  // run's loop starts; the payload rides `AgentEvent.admission`.
  | "queued"
  | "admitted"
  // Call admission: `call_queued` = one of the run's model calls waits for its
  // provider (every slot of its call lane is taken, or the provider has its cap
  // of models in flight); `call_admitted` = it got its slot. Sent by the
  // provider's call lane before the call; the payload rides
  // `AgentEvent.admission`, with `provider` and `kind` set.
  | "call_queued"
  | "call_admitted"
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
  /** How long this call took. Absent when not measured (an older server, or
   *  a call that failed). Additive + optional. */
  timing?: CallTiming;
}

/** Per-call timing, in milliseconds, on a usage event. A field that is absent
 *  (or 0) was not measured — never "took no time". */
export interface CallTiming {
  /** Wall time from the request to the model's done event. */
  duration_ms?: number;
  /** Wall time to the first text, thinking or tool event. */
  ttft_ms?: number;
  /** Server-reported phases (Ollama): model load, prompt eval, generation. */
  load_ms?: number;
  prefill_ms?: number;
  decode_ms?: number;
  /** Wall time the server did not account for — waiting behind other calls. */
  queue_ms?: number;
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

/** A sub-agent's review hold, as its parent's stream reports it. */
export interface SubagentHoldInfo {
  subagent: string;
  subagent_run_id: string;
  state: "held" | "released";
  held_by?: string;
  round?: number;
  expires_at?: string;
  /** How the child ended: completed | rejected | failed | cancelled ("released" only). */
  status?: string;
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
  review_ttl_seconds?: number;
  /** Hooks the run added to its agent's own: its request's, and a sub-agent's
   *  inherited ones. */
  hooks?: { hooks?: EventHooks; tool_hooks?: ToolHooksByTool };
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
  review?: boolean;
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

/** Input for {@link LoomcycleClient.runFile} — one File-tool op on the inbox of
 *  a run, from outside the run (`POST /v1/runs/{run_id}/file`). The run is the
 *  path's; the fields are the tool's own, snake_case as on the wire. download
 *  and upload are not offered here: they need a run's HTTP allowlist. */
export type RunFileInput = {
  op: "stat" | "ls" | "extract" | "import" | "keep" | "rm" | "clear";
  /** stat / ls / rm / extract / import: `volume:inbox:<path>`, or a bare path in the inbox. */
  path?: string;
  /** keep: the inbox file to copy. */
  from?: string;
  /** keep: `volume:<name>:<path>` in a read-write dynamic volume of your tenant. */
  to?: string;
  overwrite?: boolean;
  recursive?: boolean;
  /** clear: one folder of the inbox to empty instead of all of it. */
  prefix?: string;
  images?: boolean;
  max_rows?: number;
  excerpt_chars?: number;
  title?: string;
  doc_path?: string;
  scope?: string;
  tags?: string[];
  /** import: where the document came from, kept on it as a link back. */
  source?: RunFileImportSource;
  /** import: headings to add by line number, so a file whose structure is only
   *  in its numbering imports as sections. The file is not changed, so the
   *  document keeps the record of where it came from. */
  headings?: RunFileImportHeading[];
};

/** import's `source`: the item's web link (a SharePoint `webUrl`, an email's
 *  `webLink`), never a download or upload link. */
export interface RunFileImportSource {
  /** An http(s) link, at most 2048 bytes; a signature in its query is masked. */
  url?: string;
  kind?: "sharepoint" | "email" | "attachment" | "web" | "other";
  /** One line, at most 300 characters. */
  title?: string;
}

/** One entry of import's `headings`. Lines count from 1 in the file as it is;
 *  give entries in increasing line order. */
export type RunFileImportHeading =
  /** The line itself becomes a heading; `lines` (1–10) joins a wrapped title. */
  | { line: number; level: 2 | 3 | 4 | 5 | 6; as: "heading"; lines?: number }
  /** A heading with `title` is inserted before the line, which stays body text. */
  | { line: number; level: 2 | 3 | 4 | 5 | 6; as: "marker"; title: string };

/** What {@link LoomcycleClient.downloadRunInboxFile} returns: one inbox file's
 *  bytes and what the server says about them. */
export interface RunInboxDownload {
  /** The bytes (empty for a HEAD request). */
  bytes: Uint8Array;
  /** 200, or 206 for a range. */
  status: number;
  /** The type the server served: the recorded type when it is one a browser
   *  cannot run, else `application/octet-stream`. */
  contentType: string;
  /** The file's name, from Content-Disposition. */
  filename: string;
  /** SHA-256 (hex) of the file's bytes as served — the whole file, also for a range. */
  sha256: string;
  /** The producer the runtime recorded (`mcp:<server>/<tool>`, `http`, …), or
   *  `unknown` when the file changed since it was kept. */
  origin: string;
  headers: Headers;
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

/** Why a per-run override is refused, in {@link OverridableField.reason}:
 *  - `token_locked` — the caller's token carries an overrides policy that
 *                     leaves the field out
 *  - `agent_locked` — the agent's definition says `allow_overrides: false`
 *
 *  When both apply the token is named, because a run setting the field is
 *  refused for that reason first. */
export type OverrideLockReason = "token_locked" | "agent_locked";

/** One per-run override in {@link AgentOverridableResponse.fields}. */
export interface OverridableField {
  /** Whether a run of the agent, started by this caller, may set it. */
  overridable: boolean;
  /** Present when `overridable` is false. */
  reason?: OverrideLockReason;
  /** The run may only make it smaller than the definition allows
   *  (`max_concurrent_children`, `interruption`). */
  narrow_only?: boolean;
  /** The largest value accepted (`max_concurrent_children`: the fan-out
   *  ceiling, the definition's or the runtime default). */
  max?: number;
  /** The values accepted, when the run path checks against a closed set this
   *  agent has: `effort`, the configured `tier`s, and the `provider` and
   *  `model` sets the definition declares. Absent means no closed set here —
   *  any value the deployment accepts (see `listModels()` / `getRouting()`),
   *  not anything at all. Each describes the field set on its own. */
  values?: string[];
}

/** The reply from `GET /v1/_agentdef/{name}/overridable` — for every per-run
 *  override, whether a run of this agent started by THIS caller may set it.
 *
 *  Folds the definition's `allow_overrides` with the caller's token overrides
 *  policy, through the same checks the run path refuses with. The answer is
 *  the caller's: do not reuse it for another token. Keys of `fields` are wire
 *  names (`max_tokens`, not `maxTokens`), one per run option a token's
 *  overrides policy can name. */
export interface AgentOverridableResponse {
  agent: string;
  /** The AgentDef version answered for; absent for a yaml or registered agent,
   *  which have no versions. Always the active version: a run cannot pin one. */
  def_id?: string;
  /** The definition's `allow_overrides`; unset reads as true. */
  allow_overrides: boolean;
  /** True when the caller's token carries an overrides policy, so `fields`
   *  reflects this caller and not only the agent. */
  restricted_by_token: boolean;
  fields: Record<string, OverridableField>;
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

/** One tool call: the tool's name and its argument object. */
export interface CallRef {
  tool: string;
  input: unknown;
}

/** What a correct call to a documented tool looks like, taken from the
 *  operation's help article. */
export interface CallFormat {
  tool: string;
  /** The operation the example is for; absent when the call named no valid
   *  op, in which case `operations` lists the ones that exist. */
  op?: string;
  /** The argument object of one correct call. */
  example?: unknown;
  /** The tool's valid operations — only when the op was missing or unknown. */
  operations?: string[];
  /** The call that returns the full article. */
  reference?: CallRef;
}

/** The machine-readable half of a failure. Carried on `event: error` frames (a
 *  terminal run failure) and on a failed `event: tool_result` frame.
 *
 *  Once the SSE stream is open the HTTP status is already 200, so without this
 *  a consumer's only signal is the event type plus an English string —
 *  "retry shortly" and "your budget is exhausted" look identical and want
 *  opposite responses. */
export interface ErrorInfo {
  /** "transient" | "validation" | "business" | "permission", or "cancelled"
   *  on the error frame of a run stopped before it finished (nothing failed).
   *  Absent only on a failed tool call not yet classified that still carries
   *  `correct_call_format`. */
  category?: string;
  /** Answers only "will resending this exact call fail?" — not "should I give
   *  up". A false here still leaves the alternatives in `description` open. */
  is_retryable: boolean;
  /** What went wrong AND what to do next. */
  description?: string;
  /** Backoff hint, meaningful only when `is_retryable`. ABSENT rather than 0
   *  when there is no hint: an absent hint and a zero one are opposite
   *  instructions. */
  retry_after_ms?: number;
  /** The correct shape of the call that failed, on a failed tool call whose
   *  shape may be the cause. */
  correct_call_format?: CallFormat;
}

/** One visit of a team walk to a `decision` state (the `team_decision`
 *  payload), recorded on the walk's own run. A decision state starts no
 *  member run, and a walk started with `mode: "detach"` returns no steps, so
 *  this is where a client holding the walk's `run_id` finds the answer. */
export interface TeamDecisionInfo {
  /** The decision state's id. */
  state: string;
  /** The walk's ordinal of state visits at this one: 1 for the walk's first
   *  state, never reused, and the number a member run of the walk carries as
   *  `parent_context.state_visit`. Two passes through one state are two
   *  events with different visits. */
  visit: number;
  /** The transition the walk took out of the state: `conditional:<option>`,
   *  `conditional:true` / `conditional:false`, or `success` when the answer
   *  had no transition of its own or the state routes on nothing. */
  edge: string;
  /** The state that transition leads to. */
  next: string;
  /** What the model answered: the object a waited-for walk's step carries as
   *  `answer`. */
  answer: TeamDecisionAnswer;
}

/** What one hook did to one call, or to the run (the `hook_decision`
 *  payload). */
export interface HookDecisionInfo {
  /** `"<owner>/<name>"`. */
  hook: string;
  phase: HookPhase;
  /** The call a tool hook decided on; absent for agent_start / agent_stop. */
  tool_use_id?: string;
  tool_name?: string;
  /** The message a `channel_publish` hook decided on. */
  channel?: string;
  message_id?: string;
  /** `block` and `hold` are agent_stop's: the model was sent back with
   *  `reason`, or the answer is held for a verdict. `release`,
   *  `rewrite_body` and `drop` are a channel hook's (it may `hold` too). */
  decision:
    | "deny" | "rewrite_input" | "rewrite_output" | "context" | "block" | "hold" | "unavailable"
    | "release" | "rewrite_body" | "drop";
  /** For `unavailable`: `open` (the call went ahead) or `closed` (refused). */
  fail_mode?: "open" | "closed";
  /** A deny's or block's text, a hold's reason, or the error that made the
   *  hook unavailable. */
  reason?: string;
  /** For `rewrite_input`: the input the tool actually ran with. */
  updated_input?: unknown;
  /** For `context`: what was appended to the tool result (or, for
   *  agent_start, to the prompt). */
  additional_context?: string;
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

/** AdmissionInfo accompanies `event: queued` and `event: admitted` frames.
 *  Mirrors providers.AdmissionInfo. */
export interface AdmissionInfo {
  /** On `queued`: why the run waits — `"user_cap"` (its user already has its
   *  cap of runs active) or `"global_cap"` (every run slot is taken). On
   *  `call_queued`: why the call waits — `"lane"` (every slot of its call lane
   *  is taken) or `"models_in_flight"` (the provider has its cap of models in
   *  flight and this call's model is not among them). */
  reason?: "user_cap" | "global_cap" | "lane" | "models_in_flight";
  /** On `queued`: the run's place when it was queued, 1 being the next to
   *  start. For `user_cap` it counts only the same user's waiting runs. It
   *  moves as other runs start and end; `getRun` reports the current place
   *  (`Agent.queue_position`). */
  position?: number;
  /** On `admitted`: how long the run waited for its slot, in milliseconds. On
   *  `call_admitted`: how long the call waited. */
  wait_ms?: number;
  /** On `call_queued` / `call_admitted`: the provider id as configured. */
  provider?: string;
  /** On `call_queued` / `call_admitted`: the call lane's model kind (`"chat"`). */
  kind?: string;
}

/** Payload on `event: file_captured`: a file a tool returned, as the runtime
 *  kept it in the run's inbox volume. Never the bytes, never a URL's query. */
export interface FileCapturedInfo {
  /** The file's reference, `volume:inbox:<dir>/<name>`. */
  ref: string;
  media_type: string;
  /** Length in bytes. */
  size: number;
  /** Hex SHA-256 of the bytes. */
  sha256: string;
  /** Which producer returned it: `mcp:<server>/<tool>`, `http`, `webfetch` or
   *  `download:<host>`. */
  origin: string;
  /** The run whose tool call returned it (a sub-agent's capture is reported
   *  on the tree's stream). */
  run_id?: string;
}

/** Payload on `event: transcription`: one OCR or speech-to-text pass a File
 *  tool call made over an inbox file. Never the transcribed text. */
export interface TranscriptionInfo {
  /** The source file, `volume:inbox:<dir>/<name>`. */
  ref: string;
  /** The Markdown the pass wrote, when it wrote one. */
  out?: string;
  /** `"ocr"` or `"stt"`. */
  purpose: string;
  /** The configured engine that ran: `"vision"`, `"openai-audio"`,
   *  `"command"`, …. */
  engine: string;
  /** The concrete provider/model pair the engine called, when it called one. */
  provider?: string;
  model?: string;
  /** Pages sent to an OCR engine. */
  pages?: number;
  /** Audio duration sent to a speech-to-text engine, in seconds. */
  seconds?: number;
  input_tokens?: number;
  output_tokens?: number;
  /** The priced cost in `currency`. Absent when the pair is unpriced, which is
   *  not the same as free. */
  cost?: number;
  currency?: string;
  /** `"ok"`, `"partial"` (a cap or an engine error stopped it after some
   *  units; what was written stays) or `"failed"`. */
  status: string;
  /** An estimate: no engine was called and nothing was spent. */
  dry_run?: boolean;
  /** The run whose tool call made the pass. */
  run_id?: string;
  /** Why the pass failed or stopped early (set on `"failed"`, and on
   *  `"partial"` when a cap or an engine error stopped it). Never a secret, a
   *  URL's query or the file's content. */
  error?: string;
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
   *  "reasoning_keep" | "denied_by_hook" | "noop" | "noop_keep_spans_all" |
   *  "noop_not_smaller". */
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

/** Payload on `spawn_child_started` / `spawn_child_result` /
 *  `spawn_child_read` — the ledger a parent writes so its children survive a
 *  pause: a fan-out's in-flight children, and its poll-mode children (`mode:
 *  "poll"`) with how each ended and whether the parent has read it. */
export interface SpawnChildInfo {
  tool_use_id: string;
  index: number;
  run_id?: string;
  agent?: string;
  /** "poll" for a poll-mode child; absent for a wait-mode fan-out child. */
  mode?: string;
  batch_id?: string;
  /** "team" for a team walk run in poll mode. */
  kind?: string;
  team?: string;
  def_id?: string;
  /** The child was started with notify: false / on_parent_end: "cancel". */
  no_notify?: boolean;
  cancel_on_parent_end?: boolean;
  /** A poll-mode sub-agent's per-child timeout_ms, on its started row. */
  timeout_ms?: number;
  ok?: boolean;
  output?: string;
  error?: string;
  state?: Record<string, unknown>;
  /** A poll-mode child's final state (completed, failed, cancelled, timeout). */
  ended?: string;
  status?: string;
  /** A team walk's whole answer, steps included. */
  detail?: Record<string, unknown>;
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
  /** Payload on `event: awaiting_review` — the run is held for a verdict.
   *  `round` is 1 on the first answer and counts up with each revision.
   *  `held_by` names the agent_stop hook that held it; absent when review
   *  arming did (and only that kind is released by disarming review). */
  awaiting_review?: { since_turn?: number; round?: number; expires_at?: string; held_by?: string };
  /** Payload on `event: subagent_hold` — on a parent's stream, a sub-agent it
   *  started through the Agent tool is held for a verdict (`state` "held";
   *  rule on it with {@link LoomcycleClient.reviewRun} on `subagent_run_id`)
   *  or has ended (`state` "released", with `status`). */
  subagent_hold?: SubagentHoldInfo;
  /** Payload on `event: awaiting_children` — the background children the
   *  run waits for before its next turn. */
  awaiting_children?: { child_run_ids?: string[]; since_turn?: number };
  /** Payload on `event: children_note` — the note's text, as the model reads it,
   * and the background children it reports. */
  children_note?: { text?: string; child_run_ids?: string[] };
  /** Payload on `event: steer` (RFC AI) — the operator's drained turn. On a
   *  re-attach replay, `source` is `"replay"`. Nil on all other event types. */
  user_input?: { text?: string; source?: string; seen_at?: string };
  /** Payload on `event: limit` (RFC AW) — a per-scope token-budget crossing.
   *  Nil on all other event types. */
  limit?: LimitInfo;
  /** Payload on `event: file_captured` — a file a tool returned, kept in the
   *  run's inbox. Nil on all other event types. */
  file?: FileCapturedInfo;
  /** Payload on `event: transcription` — one OCR or speech-to-text pass a
   *  File tool call made. Nil on all other event types. */
  transcription?: TranscriptionInfo;
  /** Payload on `event: queued` (why the run waits, and its place) and
   *  `event: admitted` (how long it waited), and on `event: call_queued` /
   *  `event: call_admitted` (the same for one model call, with its provider
   *  and lane kind). Nil on all other event types. */
  admission?: AdmissionInfo;
  /** Set on `capability_inert` events: a tool the agent holds and cannot use. */
  capability_inert?: CapabilityInertInfo;
  /** Payload on `event: hook_decision`. */
  hook_decision?: HookDecisionInfo;
  /** Payload on `event: team_decision`: one visit of a team walk to a
   *  decision state. Read it from the WALK's run, with
   *  {@link LoomcycleClient.streamRunByID}: while the walk runs, and after
   *  it has ended. */
  team_decision?: TeamDecisionInfo;
  /** Set on `event: override` frames — what the operator changed, and from
   *  what to what. */
  override?: OverrideInfo;
  /** Payload on `event: error` — the classification of a TERMINAL run failure
   *  — and on a failed `event: tool_result`: its classification and the
   *  correct call format. Absent on every other event type, and absent for a
   *  failure the runtime cannot categorise: there is deliberately no "unknown"
   *  category, so a missing `error_info` means "not classified", never
   *  "classified as nothing in particular". */
  error_info?: ErrorInfo;
  // v0.4 `event: agent` side-channel announces the run's tracking IDs
  // immediately after the `event: session` frame. parent_agent_id is null
  // for top-level runs.
  agent_id?: string;
  run_id?: string;
  session_id?: string;
  parent_agent_id?: string | null;
  /** On the `agent` frame: the request's `idempotencyKey` was already held by
   *  a run, and this stream is that existing run's. */
  deduplicated?: boolean;
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
  /** Payload on `spawn_child_started` / `spawn_child_result` / `spawn_child_read`. */
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
  // `kind` labels the source: untrusted, web_content, uploaded_cv,
  // qa_question, user_input, tool_output, search_result, or code (kept exactly
  // as written, fenced by a boundary derived from the text, instead of having
  // `<` escaped); any other value is fenced as untrusted.
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

  /** Hold the run for an operator's verdict each time its model finishes,
   *  instead of completing: {@link LoomcycleClient.reviewRun} approves it, or
   *  rejects it with feedback it revises from (and is held again), or rejects
   *  it outright (status `rejected`). Settable while the run is going; `false`
   *  on a run that is held releases it as approved. */
  review?: boolean;

  /** Narrow whether this run's agent may ASK a human a question. `enabled`
   *  must be true to keep asking (false switches it off); `kinds` keeps only
   *  those the definition also allows; `max_pending` takes the smaller. It
   *  never grants the Interruption tool to an agent that does not hold it —
   *  there it is reported as `capability_inert`. A retune reaches a parked run
   *  at its next operator turn. */
  interruption?: { enabled?: boolean; kinds?: string[]; max_pending?: number };

  /** Per-run tool choice: whether and which tool the model must call, and
   *  for how many calls. REPLACES the agent's own whole. On a retune it
   *  replaces the run's own and takes effect from the run's next turn, where
   *  its `until` starts counting; mode `auto` removes the forcing. */
  toolChoice?: ToolChoiceOptions;
  /** Per-run answer schema: the final answer is held to it where the model
   *  supports it, and returned parsed as `result.structured`. REPLACES the
   *  agent's own whole. On a retune it replaces the run's own and takes
   *  effect from the run's next turn; a retune cannot remove it. */
  outputFormat?: OutputFormatOptions;
  /** The run's own `routing:` block: how the agent's tier candidates are
   *  walked — at the run's start, and when the run, having lost its lane
   *  slot, would wait too long for its next call. REPLACES the agent's block
   *  whole — `{}` turns queue-aware routing off. It chooses only among the
   *  candidates the definition declares. `queue_aware` on an agent (or a run)
   *  pinned to a provider or model is refused (400). On a retune it replaces
   *  the run's own and is read at the run's next call boundary. A sub-agent
   *  of the same definition inherits it. */
  routing?: AgentRouting;

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
  /** Continue an existing session. When set, the run is a continuation,
   *  exactly as {@link LoomcycleClient.continueSession} starts one: the
   *  session's transcript is replayed to the model, and the run uses the
   *  session's agent and identity. `agent` must then name the session's own
   *  agent; a different one is refused (400). When empty, a fresh session is
   *  created and announced as the first SSE frame. */
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
  /** Makes starting this run safe to retry: 1 to 200 characters of
   *  `[A-Za-z0-9:._-]`. A second request with the same key, from the same
   *  tenant and user, starts nothing. `runStreaming` then yields the existing
   *  run's stream — its `agent` frame carries `deduplicated: true` and its
   *  events follow from the first — and a `spawnRunBatch` child reports the
   *  existing run with `deduplicated: true`. The request body is not
   *  compared, so build the key from what makes the work distinct. A fresh
   *  run only: refused with `sessionId`. */
  idempotencyKey?: string;
  /** Bound how long the run may LIVE, in seconds (omitted or 0 = no bound; at
   *  most 30 days). Past it the run is cancelled, with what it started, and
   *  ends `cancelled` with `stop_reason: "wall_limit"`. Waits count; time the
   *  runtime is paused does not. It belongs to the run, not to the call, so it
   *  is the time bound for a `mode: "detach"` batch child, where `timeoutMs`
   *  is refused. Any agent, model-driven or code-js. */
  maxWallSeconds?: number;
  /** What the run does when it cannot start at once: `"wait"` (the default)
   *  accepts it with status `queued` — the stream yields a `queued` event,
   *  then `admitted` when it gets a slot — and `"reject"` refuses it at once
   *  (`per_user_quota_exhausted` or `backpressure`). A fresh run only. */
  admission?: "wait" | "reject";
  /** `"none"` makes this a no-trace run: what it says is delivered on this
   *  stream and stored nowhere — the server keeps a marker for each event
   *  (its type and size) and a class for an error, never the text. Omit (or
   *  `"default"`) to leave the mode to the agent and your token, either of
   *  which may still make the run no-trace: the strictest wins.
   *
   *  A no-trace run cannot be interactive, held for review, put in an
   *  existing session, continued, compacted or retuned; each is refused with
   *  `retention_none_unsupported`. A detached one returns its id and nothing
   *  readable afterwards, so the agent must deliver its own result. */
  retention?: "none" | "default";
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
  /** With `review`: end a held answer that gets no verdict within this many
   *  seconds as rejected (stop reason `review_expired`). Each hold gets the
   *  full window; omit for no deadline. Set at start only. */
  reviewTtlSeconds?: number;
  /** Hooks this run adds to its agent's own, by event. Only ever added —
   *  nothing here removes a hook the agent carries — and sub-agents the run
   *  starts inherit them. They never widen hosts. Set at start only. */
  hooks?: EventHooks;
  /** Hooks for one of the agent's tools, by tool name. Set at start only. */
  toolHooks?: ToolHooksByTool;
  /** Per-run LLM sampling override (v0.28.0), merged PER FIELD over the
   *  agent's own sampling (this wins; unset fields inherit). Omitted =
   *  inherit entirely. */
  sampling?: SamplingOptions;
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

/** The JSON schema a run's final answer must follow (RFC DI) — structured
 *  output. Same shape per-agent (AgentDef `output_format`) and per-run; a
 *  per-run value REPLACES the agent's whole. The parsed answer is
 *  `result.structured`. A model that cannot enforce it runs anyway, with a
 *  `capability_inert` event naming what was not enforced. */
export interface OutputFormatOptions {
  /** The only kind, and the default when omitted. */
  type?: "json_schema";
  /** Label where a provider needs one (OpenAI does). Default "output". */
  name?: string;
  /** A JSON Schema whose root is `type: "object"`. */
  schema: Record<string, unknown>;
}

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

/** Per-run LLM sampling override (v0.28.0). Mirrors config.Sampling — every
 *  field optional; an unset field inherits the agent's value. An explicit
 *  `temperature: 0` is deterministic, NOT "unset". Each provider maps what it
 *  supports (e.g. topK is Anthropic/Gemini/Ollama; frequencyPenalty/
 *  presencePenalty/seed are OpenAI/DeepSeek/Ollama). */
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
   *  A run a member starts (a sub-agent, at any depth) carries the member's
   *  `walk_id`, `state` and `state_visit` too, so it is in
   *  {@link LoomcycleClient.listWalkRuns} and on the walk-filtered stream.
   *  It is told from a member by `parent_run_id`: a member's is the walk's
   *  run id, a sub-agent's is the run that started it. The wave fields stay
   *  the member's alone.
   *
   *  Absent on any run outside a team walk. */
  walk_id?: string;
  wave_id?: string;
  /** Position within the wave, 0-based. Sent even when 0 (unlike the string
   *  fields, which are omitted when empty), so an index of 0 is a real first
   *  position and not an absent one. */
  wave_index?: number;
  /** Which state of the walk ran this run — set on EVERY run a team walk
   *  spawns (agent, parallel, consolidator and starter members alike), where
   *  the wave fields are a starter's alone. A member's sub-agent carries the
   *  member's. */
  state?: string;
  /** The walk's own ordinal of state visits (1, 2, …): the runs of one visit
   *  share it, and a revisit of the same state gets a new one. Absent outside
   *  a walk. */
  state_visit?: number;
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
  /** With `review`: end a held answer that gets no verdict within this many
   *  seconds as rejected (stop reason `review_expired`). Each hold gets the
   *  full window; omit for no deadline. Set at start only. */
  reviewTtlSeconds?: number;
  /** Hooks this run adds to its agent's own, by event. Only ever added —
   *  nothing here removes a hook the agent carries — and sub-agents the run
   *  starts inherit them. They never widen hosts. Set at start only. */
  hooks?: EventHooks;
  /** Hooks for one of the agent's tools, by tool name. Set at start only. */
  toolHooks?: ToolHooksByTool;
  /** Per-continuation LLM sampling override — see {@link RunOptions.sampling}. */
  sampling?: SamplingOptions;
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

  /** A source of short-lived bearers, instead of `authToken` (pass one or
   *  the other): every request asks it for the token, and a 401 is retried
   *  once with the token it hands back. An app signed in through loomcycle
   *  passes an `OAuthTokenProvider`, which refreshes before expiry. */
  tokenProvider?: TokenProvider;

  /** Custom fetch implementation; defaults to global `fetch`.
   *  Useful for testing (vi.fn()) or for runtimes that ship their
   *  own fetch (e.g. node-fetch on older Node). */
  fetch?: typeof fetch;
}

// ---- Agent metadata ----

/** `configured` is a run created but not yet started (RFC DI). */
/** `rejected` is a run a reviewer turned down without feedback (RFC DJ). */
/** `queued` is a run accepted but waiting for a run slot. */
export type AgentStatus = "configured" | "queued" | "running" | "completed" | "failed" | "cancelled" | "rejected";

/** What a running run is blocked on: an open Channel subscribe, an open
 *  Interruption ask, a hold for an operator's review verdict, an
 *  interactive run parked for the operator's next turn (`input`), or a run
 *  that ended its turn and waits for its background (poll-mode) children
 *  to end (`children`). */
export type AwaitedState = "channel" | "interrupted" | "review" | "input" | "children";

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
  /** The run that spawned this one (an Agent-tool spawn, a team-walk member,
   *  a walk started by an agent). Absent for a top-level run. Unlike
   *  parent_agent_id it names one run: an agent id is reused by every run of
   *  that agent. */
  parent_run_id?: string;
  user_id: string;
  status: AgentStatus;
  started_at: string;
  completed_at: string | null;
  stop_reason: string | null;
  /** `"none"` for a no-trace run: its transcript holds markers, not content,
   *  and `error` is a class. Absent on every other run. */
  retention?: string;
  error: string | null;
  usage: AgentUsage;
  last_heartbeat_at: string | null;
  live: boolean;
  /** A persistent interactive run — it parks at end_turn waiting for operator
   *  input. Absent for ordinary runs. */
  interactive?: boolean;
  /** What a RUNNING run is currently blocked on; absent while it makes
   *  progress and for every run that is not running. See {@link AwaitedState}. */
  awaited_state?: AwaitedState;
  /** The channel name (`awaited_state: "channel"`; a fan-in `Channel` await
   *  lists its channels comma-separated, bounded), interruption kind
   *  (`awaited_state: "interrupted"`), the agent_stop hook holding a review
   *  (`awaited_state: "review"`) or the child run ids, comma-separated and
   *  bounded (`awaited_state: "children"`) the run waits on. */
  awaited_on?: string;
  /** Where a `queued` run stands now: 1 is the next to start. For
   *  `queue_reason: "user_cap"` it counts only the same user's waiting runs.
   *  {@link LoomcycleClient.getAgent} / getRun only, from the replica that
   *  holds the run's wait; absent on every other run. */
  queue_position?: number;
  /** Why a `queued` run waits: its user already has its cap of runs active
   *  (`user_cap`), or every run slot is taken (`global_cap`). */
  queue_reason?: "user_cap" | "global_cap";
  /** The replica owning the run's live cancel handle. Absent in a
   *  single-replica deployment. */
  replica_id?: string;
  /** This run is a LIVE resident interactive sub-agent (opened with Agent
   *  op=open) on the replica that answered. HTTP listings only — the gRPC read
   *  cannot reach the per-replica registry this comes from. */
  resident?: boolean;
  /** A resident sub-agent's current state; set only with `resident`. */
  resident_state?: "running" | "awaiting_input" | "completed" | "failed";
  /** Opaque caller-tracking lineage this run carries (inherited from its
   *  root for sub-agents), v0.12.x. Echoed here alongside `usage` so you
   *  can attribute a child sub-agent's cost to the user-initiated request
   *  in a single fetch. Omitted when the run carried no context. */
  parent_context?: ParentContext;
  /** The run's answer (RFC DI) — what the row had no field for; `stop_reason`,
   *  `error` and `usage` are above. Present on {@link LoomcycleClient.getAgent}
   *  and {@link LoomcycleClient.getRun} only (listings omit it, except a walk's
   *  own row in {@link LoomcycleClient.listWalkRuns}), and only once the run has
   *  finished with something to report. */
  result?: RunResult;
  /** The run's own configuration (RFC DI): the overrides it ran with, merged
   *  over its definition. {@link LoomcycleClient.getAgent} / getRun only, like
   *  `result`; absent when the run overrode nothing. */
  spec?: RunSpec;
  /** A CONFIGURED run's request as it will start (RFC DI) — what
   *  {@link LoomcycleClient.updateConfiguredRun} edits. Snake_case wire keys;
   *  present only while the run is configured, and never holds a secret. */
  draft?: Record<string, unknown>;
}

/** A run created without being started (RFC DI). */
export interface ConfiguredRun {
  run_id: string;
  agent_id?: string;
  session_id?: string;
  status: "configured";
  /** The stored request, snake_case wire keys. */
  draft: Record<string, unknown>;
}

/** The fields {@link LoomcycleClient.updateConfiguredRun} may replace: a run's
 *  options minus its agent, identity, secrets and transport knobs. */
export type ConfiguredRunPatch = Partial<
  Omit<
    RunOptions,
    "agent" | "sessionId" | "tenantId" | "userId" | "agentId" | "userBearer" | "userCredentials" | "signal" | "debug"
  >
>;

/** A run's own configuration record (RFC DI), as persisted — the values it
 *  ran with after its per-run overrides were merged over its definition.
 *  Keys and nested values are the server's snake_case wire names (not the
 *  camelCase request options), so nested blocks are left untyped. */
export interface RunSpec {
  /** `"none"` for a no-trace run, whose record holds its mode and none of
   *  its settings: such a run stores no content and is never resumed. Absent
   *  on every other run. */
  retention?: string;
  sampling?: Record<string, unknown>;
  tool_choice?: ToolChoiceOptions;
  output_format?: OutputFormatOptions;
  compaction?: Record<string, unknown>;
  context?: Record<string, unknown>;
  max_context_tokens?: number;
  run_timeout_seconds?: number;
  /** The run's own lifetime limit, in seconds, when it was started with
   *  `maxWallSeconds`: past it the run ends `cancelled` with
   *  `stop_reason: "wall_limit"`. Absent when the run has no such limit. */
  max_wall_seconds?: number;
  /** The run started with its tree's ephemeral volumes read-only: its own
   *  definition, or an ancestor's, says `ephemeral_volumes: "ro"`. A resume
   *  restores it whatever the definition says by then. */
  ephemeral_read_only?: boolean;
  review_ttl_seconds?: number;
  /** Which model serves the run: model / provider / tier / effort. */
  routing?: Record<string, unknown>;
  /** The run's own `routing:` block ({@link RunOptions.routing}), when it
   *  set one. Its own key because `routing` above names the model choice. */
  queue_routing?: AgentRouting;
  /** The run's budget: max_tokens / max_iterations / … */
  resources?: Record<string, unknown>;
  tuning?: Record<string, unknown>;
  /** Whether the run parks at its turn boundaries (current, after any retune). */
  interactive?: boolean;
  /** Whether the run is held for review when it finishes (current, after any retune). */
  review?: boolean;
  interruption?: Record<string, unknown>;
  /** The caller's host narrowing. */
  hosts?: Record<string, unknown>;
  /** Hooks the run added to its agent's own (its request's, and a
   *  sub-agent's inherited ones). */
  hooks?: { hooks?: EventHooks; tool_hooks?: ToolHooksByTool };
  /** Hooks a definition added to the run (a team state's), each with the
   *  definition it came from: its owner (`team:<name>`), owning tenant and
   *  whether an operator wrote it. Written by the server only — no run
   *  request can set a source. */
  sourced_hooks?: Array<{
    source: { owner: string; tenant?: string; operator_authored?: boolean };
    hooks?: EventHooks;
    tool_hooks?: ToolHooksByTool;
  }>;
  /** What the run's hooks resolved to when it started, so a resumed run
   *  fires those and no others: a fingerprint of the agent's own hooks and
   *  the HookDef version each reference found. Present only on a run that
   *  carries hooks. */
  pinned_hooks?: { agent: string; defs?: Record<string, string> };
  /** What bounded a sub-run at its spawn beyond its own agent: its parent's
   *  volumes (names and modes; roots are resolved again on resume, never
   *  stored) and the fan-out width it inherited. Present only on a sub-run.
   *  Written by the server only. */
  spawn?: {
    volumes: {
      active: boolean;
      bindings?: Array<{ name: string; read_only: boolean; default?: boolean }>;
    };
    fanout_cap?: number;
    /** A resident child (Agent op=open), which a resume rebuilds as one. */
    resident?: boolean;
    /** Unix seconds a resident child was last polled or cancelled from a
     *  replica that does not hold it; its own replica reads it before it
     *  reaps the child as idle. */
    addressed_at?: number;
    /** The parent run's Crypto key scopes at spawn — the ceiling the child's
     *  own `crypto_scopes` were narrowed to, and are narrowed to again on
     *  resume. Absent: the parent held none. */
    crypto_scopes?: string[];
    /** The parent run's shell-credential narrowing at spawn (RFC EW): the
     *  names the child's own `shell_credentials` were narrowed to, and are
     *  narrowed to again on resume. Absent: the parent narrowed nothing. `[]`:
     *  the parent allowed none. */
    shell_credentials?: string[];
    /** The parent run's Git network grant at spawn (RFC FB): the operations
     *  the child's own `git_remote` was narrowed to, and is narrowed to again
     *  on resume. Absent: the parent had none. */
    git_remote?: Array<'fetch' | 'push'>;
    /** The override offer the child passes on to its own children: its
     *  parent's, unchanged, so a resume offers what the live child did. Only
     *  a child of the offering run's definition (`def_id`, else `agent_name`)
     *  takes it. `{}` when the parent offered nothing; absent on a sub-run
     *  recorded before the field existed. Written by the server only. */
    offered_overrides?: {
      /** The offering run's own record, as it started. */
      record?: RunSpec;
      def_id?: string;
      agent_name?: string;
      run_id?: string;
    };
  };
  /** The iteration limit the run started with. Absent for a run with none
   *  (unbounded, or an interactive run with no `max_iterations` of its own)
   *  and for a run recorded before the field existed. Written by the server
   *  only. */
  iteration_limit?: number;
  /** The agent version the run started on, which a resumed run continues on:
   *  the AgentDef version its agent name resolved to (`def_id` absent for an
   *  agent with no versions — the operator's yaml or a registered agent). For
   *  a registered agent, `registered_sha256` is a digest of the registration
   *  the run started on; a resume refuses one changed since. `static` marks a
   *  run that started on the operator's static configuration, which a resume
   *  reads again rather than a same-named agent registered or forked since.
   *  Written by the server only. */
  agent_version?: {
    def_id?: string;
    registered_sha256?: string;
    static?: boolean;
    /** Set when the run's agent is one of a team's OWN agents (declared in
     *  the team's definition, run as `<team>/<name>`): the team version it
     *  was read from. */
    team_def_id?: string;
  };
  /** On a team walk's own run: the team version it ran, its input and the
   *  arguments it was started with. Written once when the walk starts and
   *  never changed. Absent on any other run, and on a walk recorded before
   *  versions were: read that as "version not recorded", never as the
   *  team's current version. Written by the server only. */
  team?: RunSpecTeam;
  /** The team this run belongs to: present on every run in a team walk's
   *  spawn tree (its members, their sub-agents, and theirs) and on a
   *  continuation of one. Names the team VERSION whose own agents the run
   *  may start; a resume or continuation reads them from it. Absent outside
   *  any team, and on the walk's own run (see `team`). Written by the
   *  server only. */
  team_scope?: { team: string; def_id: string; def_tenant?: string };
  /** A code agent's time as of its last pause: the active time it had used
   *  against `run_timeout_seconds`, the time it had spent waiting (which
   *  never counts against that budget), and how long it had lived — waits
   *  included, runtime pauses excluded — against the operator's lifetime
   *  limit. A resumed run continues from it rather than starting fresh.
   *  Absent on a run that never paused, and on every run that is not a code
   *  agent. Written by the server only. */
  run_clock?: { active_ms: number; waited_ms: number; wall_ms: number };
  /** The runtime pauses this run lived through, oldest first: when each
   *  began (when the runtime paused) and, once it ended, when — the resume,
   *  or the run's re-dispatch after a restore or a restart. The last has no
   *  `until` while the pause lasts. A parent re-arming this run's
   *  `timeout_ms` after a resume leaves them out. Past 16 the oldest are
   *  folded into `pauses_folded`. Absent on a run that never lived through a
   *  pause. Written by the server only. */
  pauses?: { since: string; until?: string }[];
  /** The pauses folded out of `pauses`: the time, in nanoseconds, before
   *  `until` that the run's `timeout_ms` was stopped — its pauses and its
   *  review holds, an overlap counted once. Absent until the run has lived
   *  through more than 16 pauses. Written by the server only. */
  pauses_folded?: { until: string; stopped_ns: number };
}

/** What a team walk started with (`RunSpec.team`). */
export interface RunSpecTeam {
  name: string;
  def_id: string;
  version: number;
  content_sha256?: string;
  /** The team definition's tenant; absent for a shared definition. */
  def_tenant?: string;
  /** `def_id` when the caller pinned a version, `name` when it ran the
   *  active one. */
  resolved_by: "def_id" | "name";
  /** The walk's input, with secrets masked, cut at 16 KiB. */
  input?: string;
  /** The input's length in bytes as given. */
  input_bytes?: number;
  /** Set when `input` was cut. */
  input_truncated?: boolean;
  /** The variable values the caller supplied at start (`runTeam`'s `vars`),
   *  with secrets masked like `input`. Only those: a variable left at the
   *  team's default is not listed, and reads from the definition version
   *  this record names. Absent when the start supplied none. */
  vars?: Record<string, string>;
  /** How the walk was started: waited for (`sync`), handed back at once and
   *  run outside its caller (`detach`), or run as a background child of the
   *  agent run that started it (`poll`). */
  mode: "sync" | "detach" | "poll";
  /** The board binding; `resumed_from` names the state a persisted board
   *  resumed the walk from. */
  board?: { scope: string; chunk_id: string; resumed_from?: string };
  /** Breakpoints armed at start. Arming the live walk later does not change
   *  this record. */
  breakpoints?: string[];
  /** States armed for review at start. */
  review?: string[];
  review_ttl_seconds?: number;
  interrupt_on_cap?: boolean;
}

/** A finished run's answer (RFC DI). */
export interface RunResult {
  /** The text of the run's last assistant turn. */
  final_text?: string;
  /** The final structured state of a stateful run. */
  state?: Record<string, unknown>;
  /** The final answer parsed against the run's `outputFormat`; absent when
   *  the run had none or the answer was not a JSON object. */
  structured?: Record<string, unknown>;
  /** A team walk's run only: the id of the end (terminal) state the walk
   *  reached, which tells two endings of one team apart. Absent when the walk
   *  failed or was cancelled, and on every agent run. */
  terminal?: string;
}

export interface ListAgentsResponse {
  agents: Agent[];
  /** True when an `awaited_state` listing stopped before reading every
   *  running run (it reads at most 1,000), so a matching run may be missing.
   *  Absent otherwise. */
  truncated?: boolean;
}

/** {@link LoomcycleClient.listUserAgents}'s filters. */
export interface ListUserAgentsOptions {
  /** One status or several (a run in any of them is listed). Omitted → only
   *  running runs; `"all"` → every status. An unknown status is a 400. */
  status?: AgentStatus | "all" | AgentStatus[];
  /** List only running runs blocked on one of these waits. Implies running:
   *  a `status` other than `"running"` with it is a 400. */
  awaitedState?: AwaitedState | AwaitedState[];
  /** Narrows the result CLIENT-SIDE to runs whose `parent_agent_id`
   *  matches. */
  parentAgentId?: string;
  /** Super-admin tenant-focus (?tenant=, RFC L v0.17.0). Ignored
   *  server-side for a tenant principal — its own tenant is forced. */
  tenant?: string;
  signal?: AbortSignal;
}

/** {@link LoomcycleClient.listUserAgents}'s answer: the runs, still an array,
 *  carrying `truncated: true` when an `awaitedState` listing stopped before
 *  reading every running run. */
export type AgentList = Agent[] & { truncated?: boolean };

/** One page of a team walk's runs ({@link LoomcycleClient.listWalkRuns}),
 *  ordered by started_at then run id. `next_cursor` is "" on the last page. */
export interface WalkRunsPage {
  agents: Agent[];
  next_cursor: string;
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
   *  combined envelope. "detach" — resolve once every child has started or
   *  queued: each reports `status: "running"` (or `"queued"` while it waits
   *  for a run slot, starting by itself when one frees) with its `run_id`,
   *  goes on after the call, and is read with {@link LoomcycleClient.getRun}
   *  and stopped with {@link LoomcycleClient.cancelAgent}; a child that could
   *  not start is reported in its slot, as in join. */
  mode?: "join" | "detach";
  /** Optional join deadline (ms): a child still running when it elapses is
   *  cancelled and reported with a cancelled status in-envelope. Rejected
   *  (400) with `mode: "detach"`, which does not wait. */
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
  /** The child's `idempotencyKey` was already held by a run: the ids, status
   *  and result are that run's, and this request started nothing. */
  deduplicated?: boolean;
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

/** One bearer token an erasure names: its def id and name, never the bearer. */
export interface ErasureTokenRef {
  def_id: string;
  name: string;
}

/**
 * What an erasure removes from the subject's authority — in a report or a dry
 * run, what it would: the groups the subject leaves, the lists its bindings
 * (and its tokens' bindings) named, and the member tokens revoked.
 * Config-declared rows are not listed; the store cannot remove them.
 */
export interface ErasureAccess {
  groups: string[];
  lists: string[];
  tokens: ErasureTokenRef[];
}

/** Result of {@link LoomcycleClient.erasureReport} (RFC BL P5). */
export interface ErasureReport {
  tenant: string;
  subject: string;
  tier1_covered: ErasureTier;
  tier2_uncovered: ErasureTier;
  tier3_residue: ErasureResidue;
  access: ErasureAccess;
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
  /** The groups, lists and member tokens removed (or, in a dry run, that would be). */
  access: ErasureAccess;
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

/** One place a run-tree delete could not reach: a copy of the deleted
 *  content may still be there. */
export interface RunTreeNotReached {
  /** `snapshots`, `retention_exports`, `derived_facts` or `trace_index`. */
  name: string;
  /** How many items may be affected. Absent when that is not cheap to know. */
  count?: number;
  /** The count stopped at a bound: there may be more. */
  count_is_floor?: boolean;
  reason: string;
}

/** Result of {@link LoomcycleClient.deleteRunTree} and
 *  {@link LoomcycleClient.deleteSession}. */
export interface RunTreeDeletion {
  /** The root that was asked for (one of the two). */
  run_id?: string;
  session_id?: string;
  /** How many runs went: the root and its descendants. 0 when the root does
   *  not exist, was already deleted, or is not the caller's to read. */
  deleted_runs: number;
  /** Rows removed, by table (`runs`, `events`, `sessions`,
   *  `session_embeddings`, `interrupts`, `evaluations`, `memory_pending`,
   *  `trace_index`). Every table is present, at 0 when nothing went. */
  counts: Record<string, number>;
  /** What is left in place on purpose, and why (the usage ledger). */
  kept: Record<string, string>;
  /** Empty when nothing was deleted. */
  not_reached: RunTreeNotReached[];
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
/** Result of {@link LoomcycleClient.reviewRun}. */
export interface ReviewRunResult {
  run_id: string;
  decision: "approve" | "reject";
  delivered: boolean;
}

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
  /** The registered user's status; `""` for a subject seen only in runs. */
  status?: UserStatus | (string & {});
}

export interface ListUsersResponse {
  users: UserSummary[];
}

// ---- RFC BX Phase 2: tenant-owned users + delegated per-user tokens (v1.50.0+) ----

/** A first-class user's status.
 *
 *  - `active` — the user may do what its access mode and bindings allow.
 *  - `disabled` — refused everywhere; its delegated tokens are retired.
 *  - `pending` — provisioned by a sign-in gate whose new users await
 *    approval: the person can sign in, but every request is refused until
 *    {@link LoomcycleClient.approveUser} makes the user `active`. Only a gate
 *    puts a user here and only the approval takes one out, so createUser and
 *    updateUser refuse it.
 *
 *  Fields typed `UserStatus | (string & {})` still accept a status a newer
 *  server adds. */
export type UserStatus = "active" | "disabled" | "pending";

/** One first-class users-table row (POST/PATCH /v1/_users). The tenant is the
 *  authenticated principal's, or the one an admin or `auth:manage` caller
 *  names with `opts.tenant`; it is never in the body. */
export interface UserRecord {
  tenant_id: string;
  subject: string;
  display_name: string;
  access_mode: string; // "tenant" | "isolated"
  status: UserStatus | (string & {});
  created_at: string;
  created_by: string;
}

export interface CreateUserBody {
  subject: string;
  display_name?: string;
  /** "tenant" (default) collaborates on the tenant's shared primitives;
   *  "isolated" confines the member to its own user scope. */
  access_mode?: string;
  /** Default `active`. `pending` is refused (400 `invalid_status`). */
  status?: "active" | "disabled" | (string & {});
}

/** PATCH body — an omitted key leaves the column unchanged; a present key
 *  (even the empty string) is applied. */
export interface UpdateUserBody {
  display_name?: string;
  access_mode?: string;
  /** `disabled` also retires the user's delegated tokens. `pending` is
   *  refused (400 `invalid_status`), and `active` on a pending user with 409
   *  `use_approve`: call {@link LoomcycleClient.approveUser} instead. */
  status?: "active" | "disabled" | (string & {});
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
  /** Set when the token was minted with a lifetime: it stops authenticating
   *  at this instant. */
  expires_at?: string;
  /** def_ids of the user's older tokens that `maxActive` retired. */
  retired?: string[];
  /** What narrows the token below its scopes, when anything does. */
  restrictions?: TokenRestrictions;
  warning: string;
}

/** A token's restrictions. `agents` is the complete list of agents the token
 *  may start as top-level runs; a token carrying one is also limited to the
 *  run endpoints. */
export interface TokenRestrictions {
  agents?: string[];
  /** `"none"` when every per-run override is locked, or the overrides the
   *  token may still set. Absent when nothing is locked. */
  overrides?: "none" | string[];
  /** How often the token may start runs. */
  rate?: { per_minute: number; burst?: number };
  /** `"none"` when every run the token starts is a no-trace run. */
  retention?: "none";
}

/** One token row from GET /v1/_users/{subject}/tokens — METADATA ONLY (never
 *  the plaintext or hash). `active` applies the auth-layer validity rule. */
export interface UserTokenMeta {
  def_id: string;
  name: string;
  scopes: string[];
  created_at: string;
  retired_at?: string;
  expires_at?: string;
  restrictions?: TokenRestrictions;
  /** False once the token is retired or expired. */
  active: boolean;
}

export interface ListUserTokensResponse {
  subject: string;
  tokens: UserTokenMeta[];
}

/** One identity-provider account a user signs in with, from
 *  GET /v1/_users/{subject}/identities. Never a secret. */
export interface UserIdentity {
  /** The configured sign-in gate. */
  gate: string;
  /** The provider's issuer, when it reports one. */
  issuer?: string;
  /** The account's subject at the provider. */
  external_subject: string;
  email?: string;
  email_verified: boolean;
  created_at: string;
  /** Absent until the account has been used to sign in. */
  last_login_at?: string;
}

export interface UserIdentitiesResponse {
  subject: string;
  identities: UserIdentity[];
}

// ---- RFC FA: sign-in gates and the password gate's management API ----

/** A sign-in gate's kind. Fields typed with it still accept a kind a newer
 *  server adds. */
export type AuthGateKind = "token" | "password" | "oidc" | "oauth" | "external" | "device" | "passkey" | "scim" | (string & {});

/** One configured sign-in gate, from GET /v1/_authgate. Never key material. */
export interface AuthGate {
  name: string;
  kind: AuthGateKind;
  label: string;
  /** Whether a browser can sign in through it. */
  interactive: boolean;
  /** The gate's identity map, when it has one: whether it may provision
   *  beyond the new-user floor, the tenant it maps to (or the claim naming
   *  it) and how it provisions users. */
  trusted?: boolean;
  tenant?: string;
  tenant_claim?: string;
  provision?: string;
  /** An external gate's signing key. */
  key?: AuthGateKey;
  callback_url?: string;
  discovery_url?: string;
  /** With `probe: true`: what the external gate's discovery document said. */
  probe?: AuthGateProbe;
  /** An external gate's removal timing. */
  on_removed?: AuthGateRemoval;
}

/** An external gate's key as configured: algorithm, key id and fingerprint,
 *  or for an HMAC secret the NAME of the env var holding it — never a key. */
export interface AuthGateKey {
  alg: string;
  kid?: string;
  fingerprint?: string;
  secret_env?: string;
}

/** Durations, e.g. `168h0m0s`; `erase_after` may be `never`. */
export interface AuthGateRemoval {
  unbind_after: string;
  erase_after: string;
}

export interface AuthGateProbe {
  reachable: boolean;
  /** The discovery document's HTTP status. */
  status?: number;
  /** A failure class (`unreachable`, `status`, …), never the response. */
  error?: string;
  name_matches: boolean;
  key_matches: boolean;
  capabilities?: string[];
}

export interface AuthGateListResponse {
  gates: AuthGate[];
}

/** POST /v1/_authgate/{gate}/invites. The tenant is `opts.tenant`, not a
 *  field here. */
export interface CreateInviteBody {
  /** An existing user to give a login at this gate. It keeps its access mode
   *  and groups: the invite may restate but never raise them. Omitted, the
   *  invite makes a new user when it is redeemed. */
  subject?: string;
  /** A new user's access mode, `tenant` or `isolated`; default the gate's
   *  new-user floor. Beyond the floor only on a trusted gate (403
   *  `beyond_floor` otherwise). */
  access_mode?: string;
  /** Groups for the new user; each must exist in the tenant. */
  groups?: string[];
  /** Fix the login the invitee gets; otherwise the invitee picks one (409
   *  `login_taken` if it is in use at the gate). */
  login?: string;
  email?: string;
  /** How long the link lives, as a Go duration (`72h`); default and maximum
   *  are the gate's. */
  ttl?: string;
  /** Where the person lands after redeeming: a path on the server, or one
   *  of the gate's `allowed_return_to`. */
  return_to?: string;
}

/** A one-time invite link. `url` carries the code in its fragment: hand it to
 *  the person and do not log it. It is shown once — listing invites never
 *  returns it. */
export interface CreatedInvite {
  invite_id: string;
  url: string;
  expires_at: string;
}

export type AuthInviteState = "live" | "redeemed" | "revoked" | "expired";

/** One invite from GET /v1/_authgate/{gate}/invites. Never its code. */
export interface AuthInvite {
  invite_id: string;
  gate: string;
  tenant_id: string;
  /** Set for an invite to an existing user. */
  subject?: string;
  access_mode?: string;
  groups: string[];
  login?: string;
  email?: string;
  return_to?: string;
  /** The owner invite of a tenant that has never had a user. */
  bootstrap?: boolean;
  created_by?: string;
  created_at: string;
  expires_at: string;
  state: AuthInviteState;
  redeemed_at?: string;
  redeemed_subject?: string;
  revoked_at?: string;
}

export interface AuthInviteListResponse {
  invites: AuthInvite[];
}

/** A one-time password-reset link; like {@link CreatedInvite}, `url`
 *  carries the code and is shown once. */
export interface CreatedPasswordReset {
  reset_id: string;
  url: string;
  expires_at: string;
}

/** A user's login at one gate, from GET /v1/_authgate/{gate}/identities. */
export interface GateIdentity {
  gate: string;
  login: string;
  email?: string;
  email_verified: boolean;
  created_at: string;
  last_login_at?: string;
  /** Whether the user has a password set. */
  has_password: boolean;
  /** Whether the user has a confirmed authenticator. */
  has_totp: boolean;
}

export interface GateIdentitiesResponse {
  tenant_id: string;
  subject: string;
  identities: GateIdentity[];
}

/** GET /v1/_authgate/{gate}/tenant-status: what a website needs to decide
 *  on an owner (bootstrap) invite, and nothing about who the users are. */
export interface GateTenantStatus {
  tenant_id: string;
  /** Whether the tenant has a user now. */
  has_users: boolean;
  /** Whether it has ever had one; once true, no owner invite or owner create
   *  by an `auth:manage` credential is possible again. */
  bootstrapped: boolean;
  bootstrap_invite_live: boolean;
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
  /** The deployment's access-control rollout mode: `compat`, `shadow` or
   *  `enforce`. Absent on a server that predates access-control lists. */
  acl_mode?: AclMode;
  /** The caller's groups, transitive. Absent when none were resolved. */
  groups?: string[];
  /** The version of the caller's compiled access-control set. Absent in open
   *  mode and when the set could not be resolved. */
  acl_version?: number;
  /** What the caller may reach, as the server's own gates would answer it.
   *  Absent on a server that predates the summary. */
  effective?: WhoamiEffective;
  /** How the caller signed in. Absent on a server that predates Web UI
   *  sessions. */
  auth?: WhoamiAuth;
  /** What narrows the caller's token below its scopes, when anything does. */
  restrictions?: TokenRestrictions;
}

/** GET /v1/_me's sign-in block. `mode` is always present; the rest only when
 *  a Web UI session (not a bearer) authenticated the request. */
export interface WhoamiAuth {
  /** `open` (no credential configured), `token` or `strict`. */
  mode: "open" | "token" | "strict";
  /** The configured gate the session was created through. */
  gate?: string;
  /** The authentication methods the gate used, e.g. `["bearer"]`. */
  amr?: string[];
  /** When the session's login happened (RFC 3339). */
  auth_time?: string;
  /** When the session stops authenticating unless renewed by activity. */
  session_expires_at?: string;
  /** `pending` when the session's user awaits approval: every other request
   *  the session makes is refused until the user is approved. Absent
   *  otherwise. */
  status?: "pending";
}

/** GET /v1/_me's capability summary. Bounded and non-secret: a fixed list of
 *  Web UI pages, no per-instance data. */
export interface WhoamiEffective {
  /** Per Web UI page id (`run`, `runs`, `channels`, `access`, `users`, …),
   *  whether a request to the route the page loads first would pass the
   *  route's scope gate and its access-control question in the current mode.
   *  Every page is true in open mode. */
  surfaces: Record<string, boolean>;
}

// ---- Pause / Resume / State (v0.8.17 wire shape, v0.8.18 real impls) ----

export type RuntimeStateStatus = "running" | "pausing" | "paused";

/** A timed override of an agent's no-trace mode: until `until`, runs of
 *  `agent` in `tenant` are stored like any other. It lifts a mode that came
 *  from the agent's definition or the run request, never one a token
 *  requires. */
export interface RetentionOverride {
  tenant: string;
  agent: string;
  /** RFC 3339. */
  until: string;
  set_by?: string;
  created_at?: string;
}

export interface PauseResult {
  state: string; // "paused"
  duration_ms: number;
  /** Always 0: pause cancels no tool call. Kept for wire compatibility. */
  force_cancelled_count: number;
  paused_runs_count: number;
  /** No-trace runs on the serving replica that the pause did not park: they
   *  have nothing stored to resume from, keep running, and are left out of a
   *  snapshot. Absent when there are none. */
  unpausable_runs_count?: number;
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
  /** What the capture carried that you should act on, named by location,
   *  never by value: every literal-looking credential in a captured
   *  definition (headers, URLs, stdio env/command/args), and every paused
   *  run parked on a pending interrupt. A literal travels in the snapshot as
   *  written; replace it with a `$cred:` or `${LOOMCYCLE_*}` reference (in a
   *  stdio server's env, command or args only `${LOOMCYCLE_*}` resolves).
   *  Omitted when there are none. */
  warnings?: string[];
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
  /** How many restored paused runs the server re-dispatched as live loops.
   *  Can be below `paused_runs_restored`: a run whose agent no longer
   *  resolves is flagged failed and named in `warnings` instead. */
  paused_runs_resumed?: number;
  synthesized_sessions?: number;
  transcript_events_restored?: number;
  interaction_history_restored?: number;
  warnings?: string[];
  /** Every restore counter keyed by name (`agent_defs`, `hook_defs`,
   *  `mcp_server_defs_activated`, ...) — including the ones the typed fields
   *  above never carried. A new snapshot section adds a key here. */
  restored?: Record<string, number>;
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
  /** Aim the search at part of the scope's documents: return only the chunks of
   *  documents named at or under ANY of these Path-tree paths, in the scope's own
   *  tree (sent as `under_paths`). Narrows DOCUMENT chunks only — facts and notes
   *  the same search returns pass untouched — and is applied before ranking.
   *
   *  The four document filters need a search that can return documents (no
   *  `sources`, or `sources` including `"documents"`; otherwise 400
   *  `invalid_filter`) and SQL Memory on the server (400 `filter_unavailable`).
   *  They resolve in the caller's own tenant. With a {@link documentsScope}
   *  different from `scope` they aim at that scope's documents instead. The
   *  response then carries {@link MemorySearchResponse.filter}. For chunk-level
   *  filters (type, status, tags, a validity instant) use
   *  `document({ op: "search" })`. */
  underPaths?: string[];
  /** Never return the chunks of documents named at or under these paths (sent as
   *  `exclude_under_paths`). Documents only, as {@link underPaths}. */
  excludeUnderPaths?: string[];
  /** Return only the chunks of these documents (sent as `document_ids`).
   *  Documents only, as {@link underPaths}. */
  documentIds?: string[];
  /** Never return the chunks of these documents (sent as
   *  `exclude_document_ids`). Documents only, as {@link underPaths}. */
  excludeDocumentIds?: string[];
  /** Also search the DOCUMENTS of this scope in the same call (sent as
   *  `documents_scope`): for example a user's own notes together with the
   *  tenant's reference documents. Both are ranked into one list under one
   *  `topK`, and each entry then carries {@link MemorySearchEntry.scope}. Only
   *  documents come from this scope; facts and notes still come from `scope`.
   *  The four document filters aim at this scope's documents.
   *
   *  Equal to `scope` (and {@link documentsScopeId} omitted or equal to
   *  `scopeId`), it is the same as adding `"documents"` to `sources`. It cannot
   *  be combined with `sources: ["traces"]` (400 `invalid_documents_scope`). A
   *  caller whose access does not reach that scope's documents gets 403. */
  documentsScope?: "agent" | "user" | "tenant";
  /** The subject of {@link documentsScope} (sent as `documents_scope_id`): the
   *  agent name or user id. Required when `documentsScope` is `"agent"` or
   *  `"user"` and differs from `scope`; ignored for `"tenant"`. The tenant is
   *  always the caller's own. */
  documentsScopeId?: string;
}

/** What a document filter on {@link LoomcycleClient.memorySearch} resolved to —
 *  present on the response ONLY when a filter was sent. */
export interface MemorySearchDocumentFilter {
  /** The filter as applied: paths canonicalised, blanks and duplicates dropped.
   *  Only the kinds that were sent appear. */
  applied: {
    under_paths?: string[];
    exclude_under_paths?: string[];
    document_ids?: string[];
    exclude_document_ids?: string[];
  };
  /** Documents with at least one matching chunk. 0 means the paths/ids name no
   *  document in this scope — not that nothing relevant was found. */
  matched_documents: number;
  /** Chunks the filter let through. */
  matched_chunks: number;
  /** True when the match was sent as "everything except" its complement. Always
   *  false while access-control rules can deny the caller reading part of the
   *  scope — the counts then cover only documents it may read. */
  inverted: boolean;
  /** The scope the filter resolved in — present when it aimed at a
   *  {@link MemorySearchInput.documentsScope} different from `scope`. */
  scope?: "agent" | "user" | "tenant";
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
  /** Present when the chunk was found through one of its Document derived search
   *  units — a description, claim or question generated for it — rather than its
   *  own text. The chunk appears once however many of its units matched. */
  matched_unit?: { kind: "description" | "claim" | "question"; text: string };
  /** The scope this hit came from — present on every entry when the request
   *  set a {@link MemorySearchInput.documentsScope} different from `scope`, so
   *  the caller's own memory can be told from the other scope's documents. */
  scope?: "agent" | "user" | "tenant";
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
  /** What the document filter resolved to — present only when one was sent
   *  ({@link MemorySearchInput.underPaths} and its siblings). */
  filter?: MemorySearchDocumentFilter;
  /** The second scope searched for documents and its store scope id ("" for
   *  tenant) — present only when the request set a
   *  {@link MemorySearchInput.documentsScope} different from `scope`. */
  documents_scope?: "agent" | "user" | "tenant";
  documents_scope_id?: string;
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

/** `post_failure` runs only when the tool failed, before the post chain, with
 *  the failure's classification in the payload. */
export type HookPhase =
  | "pre"
  | "post"
  | "post_failure"
  | "agent_start"
  | "agent_stop"
  | "subagent_start"
  | "subagent_stop"
  | "pre_compact"
  | "post_compact"
  | "run_end"
  /** A channel's event: runs on each message published to a channel that
   *  carries hooks, before any reader sees it. */
  | "channel_publish";

export type HookFailMode = "open" | "closed";

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
  /** Present on a signed call only: this delivery's id, inside the signed body.
   *  Dedupe on it, never on the unsigned X-Loomcycle-Delivery header copy. */
  delivery_id?: string;
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
  /** Present on a signed call only: this delivery's id, inside the signed body.
   *  Dedupe on it, never on the unsigned X-Loomcycle-Delivery header copy. */
  delivery_id?: string;
  agent: string;
  user_id?: string;
  agent_id?: string;
  tool_call: HookToolCall;
  tool_result: HookToolResult;
}

/** What a `channel_publish` webhook receives: one message, before any reader
 *  of the channel sees it. */
export interface ChannelHookCall {
  phase: "channel_publish";
  /** `"channel:<name>"`. */
  owner: string;
  hook_name: string;
  /** Present on a signed call only: this delivery's id, inside the signed body.
   *  Dedupe on it, never on the unsigned X-Loomcycle-Delivery header copy. */
  delivery_id?: string;
  channel: string;
  scope: string;
  scope_id?: string;
  /** The same on every call for this message — a retry, or after a restart. */
  message_id: string;
  published_at: string;
  published_by?: string;
  /** `"starter_sink"` for a Starter's per-run result: a drop delivers it as
   *  an error result rather than removing it. */
  origin?: string;
  /** Counts this hook's calls for this message, from 1. */
  attempt: number;
  /** The message as the previous hook in the chain left it. */
  body: unknown;
}

/** What a `channel_publish` webhook returns. An empty body releases the
 *  message as it is. `updated_body` goes with `release` only. */
export interface ChannelHookResult {
  decision?: "release" | "drop" | "hold";
  updated_body?: unknown;
  reason?: string;
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
  // row AND the directory tree. `set_mode` (with `mode: "ro"`) makes a run's
  // ephemeral volume read-only for good; it works only from inside that run.
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
    | "purge"
    | "set_mode";
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
  // VolumeDef create, ephemeral only: fill the new volume with one commit of
  // a git repository, fetched by the runtime. Works only from inside a run.
  source?: {
    git: {
      /** https URL of the repository, on a host the operator allows. */
      url: string;
      /** The full 40-digit sha-1 of the commit. */
      commit: string;
      /** Where to find the commit when the server serves none by sha. */
      ref?: string;
      /** `$cred:<name>` or `$ghapp:<name>`; must be restricted to this URL. */
      credential?: string;
      /** May only lower the operator's limit. */
      max_bytes?: number;
      /** Also write a diff from this commit: "merge-base:default" or a sha. */
      diff_base?: string;
      /** Names for the diff's files, each a plain name under `_meta/`. */
      outputs?: { diff?: string; numstat?: string };
    };
  };
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
  /** True on a version of the shared tier (a team defined with no tenant): a
   *  non-admin token may read, run and fork it but not retire or promote it.
   *  Absent on your own tenant's versions, and for a caller whose own tenant is
   *  the shared one. */
  shared?: boolean;
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
  /** True on a version of the shared tier (a team defined with no tenant): a
   *  non-admin token may read, run and fork it but not retire or promote it.
   *  Absent on your own tenant's versions, and for a caller whose own tenant is
   *  the shared one. */
  shared?: boolean;
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
  /** Whether a walk could run the deployed version: false when it references
   *  something that does not resolve (see `issues`). Absent when
   *  `deployed: false`, or when the stored definition does not parse. */
  runnable?: boolean;
  /** What the deployed version references but does not contain. Present only
   *  when there is something to report. */
  issues?: TeamIssue[];
}

/** What a {@link TeamIssue} is about. A wire enum that only grows: a kind this
 *  client does not know is still a `string`, so a new one never breaks a build. */
export type TeamIssueKind =
  | "name_invalid"
  | "parent_not_found"
  | "overlay_invalid"
  | "graph_invalid"
  | "size_cap"
  | "channel_reserved"
  | "channel_authority"
  | "acl_missing"
  | "channel_undeclared"
  | "local_channel_invalid"
  | "local_channel_missing"
  | "local_agent_invalid"
  | "local_agent_authority"
  | "local_agent_collision"
  | "local_agent_missing"
  | "local_agent_unreferenced"
  | "local_skill_invalid"
  | "local_webhook_authority"
  | "local_webhook_invalid"
  | "agent_missing"
  | "decision_authority"
  | "decision_unconfigured"
  | "decision_model_unknown"
  | "decision_limits"
  | "uncheckable"
  | (string & {});

/** One problem with a team definition, from {@link LoomcycleClient.verifyTeam}.
 *
 *  `severity`: `refused` — a create or fork would be refused (its `detail` is
 *  exactly the refusal); `unrunnable` — it would be stored, but a walk could not
 *  run it; `advisory` — worth reading, stops nothing. */
export interface TeamIssue {
  kind: TeamIssueKind;
  severity: "refused" | "unrunnable" | "advisory";
  detail: string;
  /** JSON path of the value at fault in the definition, e.g.
   *  `states[2].handler.sink.channel` or `local.agents.reviewer`. Absent when
   *  the problem is the definition as a whole. */
  path?: string;
  /** The state the problem is in, and the handler field within it. */
  state?: string;
  field?: string;
  /** What the problem names. */
  channel?: string;
  agent?: string;
  skill?: string;
  side?: "publish" | "subscribe";
  /** Set (true) on an advisory issue — the older spelling of
   *  `severity: "advisory"`, kept for clients that read it. */
  advisory?: boolean;
}

/** An unsaved team for {@link LoomcycleClient.verifyTeam} to check. */
export interface TeamDraft {
  /** Exactly what `createTeam` / `forkTeam` would be sent. */
  overlay: Record<string, unknown>;
  /** Check it as a create or as a fork. Omitted: as a save would — a fork of
   *  the active version when the name has one, else a create. */
  as?: "create" | "fork";
  /** Check it as a fork of this version. */
  parentDefId?: string;
  /** The description the save would carry (its size is checked too). */
  description?: string;
}

/** Result of {@link LoomcycleClient.verifyTeam} for a {@link TeamDraft}: every
 *  problem a create or fork would refuse, plus what would make the stored team
 *  unrunnable — all at once. Nothing is written. */
export interface TeamDraftVerification extends TeamVerification {
  /** No `refused` issue: a create or fork with this overlay would be accepted. */
  valid: boolean;
  /** Valid, and no `unrunnable` issue. */
  runnable: boolean;
  /** How the draft was checked. */
  checked_as: "create" | "fork";
  /** The version the fork would start from (`checked_as: "fork"`). */
  parent_def_id?: string;
  /** The hash the save would sign. Absent when the overlay cannot be built.
   *  `matches` compares it with the deployed version's. */
  content_sha256?: string;
  /** Every problem found, refusals first in the order a save checks them;
   *  empty when there are none. */
  issues: TeamIssue[];
}

/** One question of a `decision` state: the shape {@link DecisionQuestion}
 *  has on a decision call. */
export interface TeamDecisionQuestion {
  type: "choice" | "noul" | "score";
  /** The question itself, in plain words. */
  instructions: string;
  /** `choice` (required): each option mapped to a description, or to `null`
   *  when the option explains itself; the option names are the answers a state
   *  routes on. `noul` (optional): descriptions of `"true"` and/or `"false"`.
   *  `score` (required): level descriptions, lowest first. */
  criteria?: Record<string, string | null> | string[];
}

/** The handler of a team state of kind `decision`: one call to a decision
 *  model, with no agent run. It binds what was answered through `capture`
 *  and, when it names a `route`, takes the transition that answer selects.
 *
 *  A save refuses a routed answer with nowhere to go: every option of a routed
 *  `choice` needs a `conditional:<option>` transition, and a routed `noul`
 *  both `conditional:true` and `conditional:false`, unless the state has a
 *  `success` transition, which takes any answer without its own. */
export interface TeamDecisionHandler {
  kind: "decision";
  /** One of the operator's decision models; omitted means the default. A
   *  model this deployment does not offer is stored and reported by
   *  `verifyTeam` as `decision_model_unknown`. */
  model?: string;
  /** The JSON object the questions are about. Its string values, at any
   *  depth, are expanded when the state runs: `${var.<name>}`, `${now.*}`,
   *  `${team.*}`, and `{{thread.output}}` for what the previous state handed
   *  over. No other `{{…}}` placeholder is allowed. */
  about: Record<string, unknown>;
  questions: Record<string, TeamDecisionQuestion>;
  /** The question whose answer picks the transition: a `choice` or a `noul`.
   *  Omitted: the state advances on `success`. */
  route?: string;
  /** For a routed `noul`: the probability of yes at or above which the answer
   *  is `true`. Default 0.5; above 0 and below 1. */
  threshold?: number;
  /** Variable name → JSONPath into the answer, e.g. `$.answers.route.choice`,
   *  `$.answers.urgent.noul`, `$.answers.route.probabilities.billing`. */
  capture?: Record<string, string>;
  /** Bounds the call. 0 or omitted: the model's own timeout. */
  timeout_ms?: number;
}

/** What a `decision` state's model answered: the object a decision call
 *  returns. On a walk's step as `answer`. */
export interface TeamDecisionAnswer {
  /** The model asked, by the operator's name for it. */
  model: string;
  provider: string;
  served_model: string;
  /** One entry per question, under the question's name. */
  answers: Record<string, Record<string, unknown>>;
  usage: { input_tokens: number; output_tokens: number };
}

/** One executed state of a team walk, in {@link TeamRunResult.steps}. */
export interface TeamRunStep {
  state: string;
  /** The state's agent, for an `agent` or `consolidator` state; else "". */
  agent: string;
  /** The transition label taken, and the state it led to. */
  edge: string;
  next: string;
  /** What the state handed to the next one. A `decision`, `vars` or `channel`
   *  state hands on what it was handed. */
  output: string;
  /** A `decision` state's answer. Absent on every other kind. */
  answer?: TeamDecisionAnswer;
  [extra: string]: unknown;
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
  /** The per-state trace. Absent when `deduplicated` is set: the trace is
   *  built by the request that walks and is not stored on the run, so a retry
   *  answered from the key has none to return. */
  steps?: TeamRunStep[];
  /** Set (true) when the request's `idempotencyKey` was already held: nothing
   *  was started, and this is the walk that holds it, as it is now — `status`
   *  may be "running", and there are no `steps`. */
  deduplicated?: boolean;
  /** On a `deduplicated` answer for a walk that has ended: the stop reason
   *  recorded on its run. Absent while it runs, and when none was recorded. */
  stop_reason?: string;
  /** On a `deduplicated` answer for a walk that has ended: why it failed, as
   *  recorded on its run. Absent when it did not fail. */
  error?: string;
  [extra: string]: unknown;
}

/** What team {@link LoomcycleClient.runTeam} walks, and how. */
export interface TeamRunTarget {
  name?: string;
  defId?: string;
  /** The initial task handed to the entry state's agent. */
  input?: string;
  /** Values for this walk's variables, name → text, read by the team's
   *  prompts as `${var.<name>}`. Only a name the team's definition declares in
   *  its `vars` is accepted — any other is refused before anything runs, with
   *  the declared names. A value given here replaces the team's default for
   *  this walk; a `vars` state, a `capture` or a starter's `binds` may still
   *  overwrite it as the walk runs. Literal text: never expanded, no `{{` or
   *  `}}`, at most 4096 bytes. */
  vars?: Record<string, string>;
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
   *  `"<state>"` or `"<state>:before_dispatch"`: the walk pauses before the
   *  state dispatches its wave, with every prompt composed and nothing run.
   *
   *  A walk can also be armed AFTER it starts — see
   *  {@link LoomcycleClient.setRunBreakpoints} — which is the case this
   *  argument cannot serve: you start a run expecting it to work, watch a wave
   *  go wrong, and want to stop before the next one.
   *
   *  `"<state>:review"` arms review instead — see {@link TeamRunTarget.review}. */
  breakpoints?: string[];
  /** Starter, agent or parallel state ids whose member runs are held for an
   *  operator's verdict when they finish: approve with
   *  {@link LoomcycleClient.reviewRun}, reject with feedback the member revises
   *  from, or reject. A rejected starter member reaches the sink as status
   *  `"rejected"` and does not count toward the wave's `wait`; a rejected agent
   *  or parallel member counts as failed. A consolidator state is refused — its
   *  answer is the walk's verdict on the work, and a parallel state's own
   *  consolidator is never held. Also armable live as the breakpoint
   *  `"<state>:review"`. */
  review?: string[];
  /** With `review`: end a member hold nobody rules on within this many
   *  seconds as rejected. Omit for no deadline. Changeable while the walk
   *  runs — see {@link LoomcycleClient.setRunBreakpoints}. */
  reviewTtlSeconds?: number;
  /** Makes starting this walk safe to retry: 1 to 200 characters of
   *  `[A-Za-z0-9:._-]`. A second `runTeam` with the same key, from the same
   *  tenant and user, starts nothing and is answered with the walk the key
   *  already holds, marked `deduplicated: true`.
   *
   *  Use one key per press of Start and send it again on a retry of that
   *  press: a start whose answer was lost then returns the walk it started,
   *  not a second one. The rest of the request is not compared.
   *
   *  The answer to a retry is the walk's `run_id` and `status`, plus
   *  `final_state` and `final_output` once it has ended. It is returned at
   *  once in every mode (a retry does not wait for a walk still running), and
   *  it has no `steps`: read the walk's member runs for those. Refused with
   *  `boardChunkId`, which already resumes a stored position. */
  idempotencyKey?: string;
  /** When a state hits its iteration cap, ask a human (an interruption)
   *  whether to continue, `reroute:<state>` or abort, instead of ending with
   *  the `iteration_cap` outcome. An ask nobody answers, that times out or is
   *  declined aborts the walk. Default false. */
  interruptOnCap?: boolean;
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
  /** "running" for a walk this call started. For a retry answered from an
   *  `idempotencyKey`, the walk's status now, which may be an ended one. */
  status: string;
  /** Set (true) when the request's `idempotencyKey` was already held: nothing
   *  was started, and this is the walk that holds it. */
  deduplicated?: boolean;
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
  /** The review deadline, in seconds, a member hold beginning now gets —
   *  0 = none. Starts as the run's {@link TeamRunTarget.reviewTtlSeconds};
   *  changeable with {@link LoomcycleClient.setRunBreakpoints}. */
  review_ttl_seconds: number;
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
/** One evidence row of a Document relation (a statement). */
export type DocumentEdgeEvidence = {
  chunk_id?: string;
  session_id?: string;
  event_seq?: number;
  scope?: "agent" | "user" | "tenant";
  natural_key?: string;
  quote: string;
  char_start?: number;
  char_end?: number;
};

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
    | "get_edge"
    | "propose_statement"
    | "accept_statement"
    | "reject_statement"
    | "list_statements"
    | "maintain_statement"
    | "resolve_anchor"
    | "lease"
    | "release"
    | "describe_ontology"
    | "extract_citations"
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
  /** get_edges / backlinks / list_statements: only these relation kinds. */
  kinds?: string[];
  /** link_chunks / propose_statement: the relation's qualifiers. */
  attributes?: Record<string, unknown>;
  /** propose_statement / maintain_statement / upsert_chunk (its relation): how
   *  the statement was made (maintain_statement: the judge of what it changes) — `rule:<name>` or `extractor:<name>`, the name `[a-z0-9_./-]{1,64}`.
   *  Only a code-js agent whose `statement_methods` lists the prefix may send
   *  it; anything else is refused with `method_not_granted`. Omitted = `manual`. */
  method?: string;
  /** maintain_statement: verify | expire | demote | merge | stale_evidence |
   *  relocate_evidence | flag. */
  action?:
    | "verify"
    | "expire"
    | "demote"
    | "merge"
    | "stale_evidence"
    | "relocate_evidence"
    | "flag";
  /** maintain_statement merge: the live statement of the same from, to, kind
   *  and status that keeps the merged statement's evidence. */
  survivor_id?: string;
  /** maintain_statement stale_evidence / relocate_evidence: the evidence row
   *  (its id from get_edge). */
  evidence_id?: string;
  /** maintain_statement relocate_evidence: the chunk the row's quote is in now. */
  chunk_id?: string;
  /** link_chunks / propose_statement: the text the relation rests on. Each row
   *  names ONE source (chunk_id; session_id + event_seq; or scope + natural_key)
   *  and the verbatim quote, which the server checks against that source. */
  evidence?: DocumentEdgeEvidence[];
  /** get_edges (default true) / backlinks (default false): include relations
   *  proposed for review. graph_recall (default false): also follow them. */
  include_proposed?: boolean;
  /** get_edges / backlinks / graph_recall / list_facts: also return what has been
   *  retired (superseded relations and facts). list_statements: also list
   *  relations that have expired or whose invalid_at has passed. */
  include_retired?: boolean;
  /** upsert_chunk / link_chunks / propose_statement: 0..1, how sure the writer is.
   *  Recorded, never used to hide anything; only a judge_fact verdict withholds. */
  confidence?: number;
  /** get_edges: out | in | both (default both). reorder_chunk: up | down. sync /
   *  diff_remote: pull (default) | push. */
  direction?: "out" | "in" | "both" | "up" | "down" | "pull" | "push";
  /** get_edges / backlinks / list_statements: the next_cursor of the previous page
   *  (list_statements: with the same `order`). */
  after?: string;
  /** list_statements: only relations with an evidence row quoting a chunk of
   *  this document. (`document_id` keeps relations with either end in it.) */
  evidence_document_id?: string;
  /** list_statements: only relations whose method starts with exactly this text
   *  (case-sensitive, no wildcards), e.g. "rule:". */
  method_prefix?: string;
  /** list_statements: created (default) is newest first; verified is least
   *  recently verified first, never-verified ones before any. */
  order?: "created" | "verified";
  /** graph_recall: declared (the tenant's walk kinds) or all relations. */
  walk?: "declared" | "all";
  /** graph_recall with budget_chars: each hop spends at most decay^hop of it.
   *  Refused without budget_chars. */
  decay?: number;
  /** graph_recall: also step to the parent (up) / children (down). */
  include_hierarchy?: "up" | "down" | "both" | "none";
  /** search: after ranking, add what the top hits are related to (one hop). */
  expand?: { kinds?: string[]; hops?: 1; budget_chars?: number };
  /** related: a chunk whose neighbours BY RELATION to return as `linked`. */
  related_to?: string;
  /** import_md: the markdown is a new edition of document_id / work_key. */
  reimport?: boolean;
  /** import_md with reimport: when the edition came into force (unix nanos). */
  edition_date?: number;
  /** import_md with reimport / resolve_anchor: the work's key. */
  work_key?: string;
  /** resolve_anchor: a part of the work, by anchor or first anchor. */
  anchor?: string;
  /** resolve_anchor: file a placeholder for a work not held yet. */
  create_placeholder?: boolean;
  /** upsert_chunk: the chunk the fact was taken from (a derived_from link). */
  source_chunk_id?: string;
  /** upsert_chunk with source_chunk_id: where the quote sits (characters). */
  char_start?: number;
  char_end?: number;
  /** upsert_chunk: the subject nodes the fact is about (runtime-written `about`). */
  about_ids?: string[];
  /** upsert_chunk with two or more about_ids: the relation kind between the first two. */
  relation?: string;
  /** query_chunks / query_documents / documents_summary: restrict to documents
   *  at/under this Path-tree path. search / related: return only chunks of
   *  documents named at or under it — applied inside every retrieval leg before
   *  its limit, so the best matches come from that part of the tree.
   *  list_statements: relations with either end in a document named there. */
  under_path?: string;
  /** search / related: chunks of documents named at or under ANY of these paths
   *  (with `under_path`, the union of both). */
  under_paths?: string[];
  /** search / related: never chunks of documents named at or under this path. */
  exclude_under_path?: string;
  /** search / related: never chunks of documents named at or under these paths. */
  exclude_under_paths?: string[];
  /** documents_summary: the documents to summarize. search / related: return
   *  only chunks of these documents. */
  document_ids?: string[];
  /** search / related: never chunks of these documents (e.g. the document under
   *  review, so its own text is not found as evidence for itself). */
  exclude_document_ids?: string[];
  /** search / related: chunk types, each including its ontology subtypes (with
   *  `type`, the union of both). */
  types?: string[];
  /** search / related: exact chunk statuses; any matches (with `status`, the union). */
  statuses?: string[];
  /** graph_recall / list_facts: answer as of this instant (unix nanoseconds).
   *  search / related / query_chunks: only chunks whose stated validity (the
   *  chunk's own valid_at/invalid_at and, for a fact, its fact dates) contains
   *  it; a chunk with no stated validity always matches. */
  as_of?: number;
  /** graph_recall: how much of each chunk's text the rows carry. Omitted: chunks
   *  that are not facts carry the first 600 characters of their body (a fact's
   *  title is its claim, so facts stay title-only). `0`: titles only — the
   *  cheapest structural walk, the response shape from before bodies existed.
   *  `N` (at most 4000): up to N characters on every row, facts included. A cut
   *  body ends on a word and the row says `body_truncated: true`; `budget_chars`
   *  counts body characters too. */
  body_chars?: number;
  /** When the content became true in the world (unix nanoseconds).
   *  upsert_chunk: the fact's start. create_chunk / update_chunk: the chunk's
   *  own start (an edition coming into force); update_chunk with `null` clears
   *  it. Omit when unknown. */
  valid_at?: number | null;
  /** When the content stopped being true (unix nanoseconds, exclusive; after
   *  valid_at). Same ops as {@link valid_at}; `null` clears on update_chunk. */
  invalid_at?: number | null;
  /** query_chunks: raw read-only SELECT (escape hatch; validator-gated). */
  sql?: string;
  /** Row/response bound. On `documents_summary` it defaults to 500 (max 5000) and
   *  the response reports `truncated: true` when it clips — an `under_path` over a
   *  subject-homed fact store is as wide as the tenant's entity count, so page the
   *  directory with `path op=ls` and pass each page's ids as `document_ids`. */
  limit?: number;
  /** define/list_types: the type name. lease / release: the lease's name. */
  name?: string;
  /** lease: how long to hold it, in milliseconds (default 60000, max one hour). */
  ttl_ms?: number;
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
   *  was refused, and why. graph_recall also follows relations a judge marked
   *  unsupported, never one an operator rejected. */
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

/** What a filtered `document({op:"search"|"related", …})` call resolved its
 *  structural filter to — present on the response ONLY when a filter argument
 *  was passed (a filter-free response has no `filter` key). */
export type DocumentSearchFilter = {
  /** The filter as applied: singular fields folded into the plural ones, paths
   *  canonicalised. Only the kinds that were passed appear. */
  applied: {
    under_paths?: string[];
    exclude_under_paths?: string[];
    document_ids?: string[];
    exclude_document_ids?: string[];
    types?: string[];
    statuses?: string[];
    tag?: string;
    tag_prefix?: string;
    as_of?: number;
  };
  /** Documents with at least one matching chunk. */
  matched_documents: number;
  /** Chunks the search could return. 0 = the filter matched nothing (the
   *  response then has no hits and the query was never embedded). */
  matched_chunks: number;
  /** Present when a type was widened to its ontology subtypes. */
  type_expanded_to?: string[];
  /** True when the match was sent to the search as "everything except" its
   *  complement (the smaller list). Always false while access-control rules can
   *  deny the caller reading part of the scope — the counts above then cover
   *  only documents it may read. */
  inverted: boolean;
};

/** One hit of `document({op:"search"})`. */
export type DocumentSearchHit = {
  chunk_id: string;
  score: number;
  rank_score: number;
  title?: string;
  type?: string;
  document_id?: string;
  /** The chunk's own validity period, present only when set. */
  valid_at?: number;
  invalid_at?: number;
  matched_unit?: { kind: string; text: string };
};

/** Response of `document({op:"search"})` — narrow a {@link DocumentToolResponse}
 *  to it. */
export type DocumentSearchResult = {
  chunks: DocumentSearchHit[];
  filter?: DocumentSearchFilter;
  reranked?: boolean;
  rerank_reason?: string;
};

/** Response of `document({op:"related"})` — narrow a {@link DocumentToolResponse}
 *  to it. */
export type DocumentRelatedResult = {
  related: Array<{
    chunk_id: string;
    score: number;
    title?: string;
    type?: string;
    document_id?: string;
    valid_at?: number;
    invalid_at?: number;
  }>;
  filter?: DocumentSearchFilter;
};

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

/** Who recorded an evaluation, relative to the run it scores. The server
 *  derives it from the caller and the target run; it is never sent. A call
 *  outside a run (this client) records `external` or `unrelated`. */
export type EvaluationEmitterRole =
  | "self"
  | "sibling"
  | "parent"
  | "external"
  | "unrelated";

/** `evaluation({op:"submit"})`: record a score against a run. Scores are
 *  additive — submitting again records another evaluation. */
export interface EvaluationSubmitInput {
  op: "submit";
  run_id: string;
  /** Required. Convention [-1,1] or [0,1]; the operator chooses. */
  score: number;
  /** Named axes to numeric values, e.g. `{correctness: 0.8}`. */
  dimensions?: Record<string, number>;
  /** Free-form structured payload. */
  judgement?: unknown;
  /** Natural-language reasoning. */
  rationale?: string;
}

/** `evaluation({op:"get"})`: one evaluation by id. */
export interface EvaluationGetInput {
  op: "get";
  eval_id: string;
}

/** `evaluation({op:"list_for_run"})`: every evaluation of one run. */
export interface EvaluationListForRunInput {
  op: "list_for_run";
  run_id: string;
  /** Max rows (default 100). */
  limit?: number;
}

/** `evaluation({op:"list_for_def"})`: every evaluation of runs of one
 *  definition version, in the caller's tenant. */
export interface EvaluationListForDefInput {
  op: "list_for_def";
  def_id: string;
  /** Max rows (default 100). */
  limit?: number;
}

/** `evaluation({op:"aggregate"})`: the scores of a definition rolled up. */
export interface EvaluationAggregateInput {
  op: "aggregate";
  def_id: string;
  /** Walk parent_def_id and include the ancestors' evaluations. */
  include_lineage?: boolean;
}

/** Input for {@link LoomcycleClient.evaluation}, discriminated by `op`. */
export type EvaluationInput =
  | EvaluationSubmitInput
  | EvaluationGetInput
  | EvaluationListForRunInput
  | EvaluationListForDefInput
  | EvaluationAggregateInput;

/** What `submit` returns: the stored row's identity and the derived role. */
export interface EvaluationSubmitResult {
  eval_id: string;
  run_id: string;
  /** The definition version the run was running, denormalised at submit
   *  time; `""` for a run that resolved through static config only. */
  def_id: string;
  emitter_role: EvaluationEmitterRole;
  score: number;
  /** RFC 3339 UTC. */
  created_at: string;
}

/** One stored evaluation, as `get` and the `list_for_*` ops return it. */
export interface EvaluationRecord {
  eval_id: string;
  run_id: string;
  def_id: string;
  score: number;
  /** `null` when the submit carried none. */
  dimensions: Record<string, number> | null;
  /** `null` when the submit carried none. */
  judgement: unknown;
  rationale: string;
  emitter_role: EvaluationEmitterRole;
  emitter_agent_id: string;
  emitter_run_id: string;
  /** RFC 3339 UTC. */
  created_at: string;
}

/** What `list_for_run` returns. An unknown run, or one the caller may not
 *  read, lists nothing rather than refusing. */
export interface EvaluationListForRunResult {
  run_id: string;
  evaluations: EvaluationRecord[];
}

/** What `list_for_def` returns. An unknown definition, or one the caller may
 *  not read, lists nothing rather than refusing. */
export interface EvaluationListForDefResult {
  def_id: string;
  evaluations: EvaluationRecord[];
}

/** Summary statistics over a set of scores. */
export interface EvaluationScoreStats {
  mean: number;
  median: number;
  min: number;
  max: number;
  latest: number;
  count: number;
}

/** What `aggregate` returns. `count: 0` for an unknown definition. */
export interface EvaluationAggregate {
  def_id: string;
  count: number;
  score: EvaluationScoreStats;
  /** Keyed by dimension name; only dimensions some row supplied. */
  dimensions?: Record<string, EvaluationScoreStats>;
  /** Keyed by emitter role. */
  by_emitter_role?: Record<string, EvaluationScoreStats>;
  lineage_included: boolean;
}

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
  /** Hooks that decide each message before any reader sees it. */
  hooks?: EventHooks;
  message_count: number;
  /** Messages held for a release, and waiting for the channel's hooks —
   *  both counted in `message_count`. */
  held_count?: number;
  awaiting_hooks_count?: number;
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
  /** The channel holds: stored, not delivered until released. */
  held?: boolean;
  /** The channel carries hooks: stored, delivered only if they release it. */
  awaiting_hooks?: boolean;
  /** How many of the channel's oldest messages the write trimmed to stay
   *  within max_messages. */
  dropped_oldest?: number;
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

/** Where one of a team's own channels is declared, among the team's stored
 *  versions: by the active one, by a stored version that is neither active
 *  nor retired, or only by a retired one. */
export type TeamChannelDeclaredIn = "active" | "inactive" | "retired";

/** One of a team's own channels, as {@link LoomcycleClient.listTeamChannels}
 *  returns it: its definition, the version that declares it, and how much is
 *  on it. */
export interface TeamChannelDescriptor {
  /** The name the team gave the channel (`journal` for `./journal`). */
  name: string;
  scope: "tenant" | "user";
  semantic?: string;
  /** Publishes are stored and not delivered until released. */
  hold?: boolean;
  default_ttl?: number;
  max_messages?: number;
  /** Which version the definition above was read from. A channel's messages
   *  belong to the team, not to a version, so a channel only a retired
   *  version declares is still listed. */
  declared_in: TeamChannelDeclaredIn;
  def_id: string;
  version: number;
  /** Counts in the team's tenant. For a user-scoped channel they are summed
   *  over every user: there is no per-user figure. */
  message_count: number;
  /** Held for a release; included in `message_count`. */
  held_count?: number;
  awaiting_hooks_count?: number;
  /** RFC3339Nano; absent when nothing is deliverable. */
  oldest_visible_at?: string;
  newest_visible_at?: string;
}

/** Response shape for {@link LoomcycleClient.listTeamChannels}. `channels` is
 *  empty, never absent, for a team that declares none. */
export interface TeamChannelsResult {
  team: string;
  channels: TeamChannelDescriptor[];
}

/** Options for {@link LoomcycleClient.listTeamChannels}. */
export interface ListTeamChannelsOptions {
  /** Only for a caller who sees every tenant: the tenant that holds the team. */
  tenant?: string;
  signal?: AbortSignal;
}

/** Options for {@link LoomcycleClient.peekTeamChannel}. */
export interface PeekTeamChannelOptions {
  /** For a user-scoped channel: whose messages to read. Default: the
   *  caller's own. Another user's is read only by a caller who may read any
   *  user's channel; for anyone else the channel is not found. */
  userId?: string;
  fromCursor?: string;
  /** Default 10, at most 100. */
  maxMessages?: number;
  tenant?: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.peekTeamChannel}. Like
 *  {@link ChannelPeekResult} it carries no cursor: a peek never advances one. */
export interface TeamChannelPeekResult {
  team: string;
  name: string;
  scope: "tenant" | "user";
  declared_in: TeamChannelDeclaredIn;
  messages: ChannelMessageItem[];
}

/** Options for {@link LoomcycleClient.releaseTeamChannel}. */
export interface ReleaseTeamChannelOptions {
  /** How many held messages to deliver, oldest first. Default 1, at most 1000. */
  count?: number;
  /** For a user-scoped channel: whose held messages to deliver. Default: the
   *  caller's own. */
  userId?: string;
  tenant?: string;
  signal?: AbortSignal;
}

/** Result of {@link LoomcycleClient.releaseTeamChannel}. `released` is the
 *  ids of the messages delivered and is empty, never absent, when nothing
 *  was held. */
export interface TeamChannelReleaseResult {
  team: string;
  name: string;
  released: string[];
  released_count: number;
  still_held: number;
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
  /** As on a single publish. */
  held?: boolean;
  awaiting_hooks?: boolean;
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
  /** "global" | "tenant" | "agent" | "user". Defaults to "global" if omitted. */
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
  /** Hooks that decide each message published to the channel before any
   *  reader sees it: `{ channel_publish: [HookDef name | inline webhook] }`.
   *  Skipped while the server runs no channel hooks; an unknown HookDef is
   *  refused (400 `channel_hooks_invalid`). */
  hooks?: EventHooks;
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
  /** Replace the channel's hooks; `{}` removes them. */
  hooks?: EventHooks;
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
  /** The run that spawned this one; absent for a top-level run. Unlike
   *  `parent_agent_id`, which every run of an agent reuses, it names one run. */
  parent_run_id?: string;
  status: string;
  stop_reason?: string;
  error?: string;
  ts: string;
  /** Opaque caller-tracking lineage echoed on the transition (v0.12.x),
   *  so a subscriber learns which root request a finishing sub-agent
   *  belongs to. Omitted when the run carried no context. */
  parent_context?: ParentContext;
  /** What a `running` run has started waiting on. A wait is announced as its
   *  own `running` event and its end as another `running` event without this
   *  field, so a client folding the stream holds the run's current wait. */
  awaited_state?: AwaitedState;
  /** The channel (a fan-in `Channel` await: its channels, comma-separated),
   *  the interruption kind, the agent_stop hook holding a review, or the
   *  background child run ids (comma-separated) a run waits on. */
  awaited_on?: string;
  /** RFC3339: when a review hold ends as rejected if nobody rules on it.
   *  Absent when the wait has no deadline. */
  hold_expires_at?: string;
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
  /** The parent-run filter the server applied, echoed back like
   *  `filter_walk_id`. Empty when unfiltered, absent on a server that
   *  predates the filter (which then streams every run). */
  filter_parent_run_id?: string;
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
  /** v1.78.0 — SERVER-side filter: only one team walk — the walk's own run
   *  (the event's `run_id`) and the runs it spawned (each event's
   *  `parent_context.walk_id`).
   *
   *  A team walk's own run_id IS its walk id, so a caller that started a team
   *  with `mode: "detach"` passes back exactly the handle it already holds —
   *  there is no second identifier and nothing to map. That is what makes a
   *  live view of one running workflow's agents a filter rather than a
   *  feature.
   *
   *  Unlike {@link StreamUserRunStatesOptions.parentAgentId}, this is applied
   *  by the server, so it reduces what crosses the wire and not just what your
   *  callback sees. The server echoes it back on the open frame as
   *  {@link RunStateStreamOpen.filter_walk_id}. */
  walkId?: string;
  /** SERVER-side filter: only one run's direct children, the events whose
   *  `parent_run_id` is this run id. The run's own transitions are not in
   *  it, nor its grandchildren's. It is the live twin of
   *  {@link LoomcycleClient.listChildRuns}. With `walkId`, an event must pass
   *  both. Echoed on the open frame as
   *  {@link RunStateStreamOpen.filter_parent_run_id}. */
  parentRunId?: string;
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
  /** True on a get/list row of the shared tier (a definition made with no
   *  tenant): a non-admin token may read and fork it but not retire or promote it.
   *  Absent on your own tenant's rows, and for a caller whose own tenant is the
   *  shared one. */
  shared?: boolean;
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
  /** How the server's text results reach the model. `"passthrough"` (the
   *  default) hands them over as they came. `"capture"` keeps a file the
   *  server returns as base64 inside a text block — the whole block, or a
   *  JSON string field such as Microsoft Graph's `contentBytes` — in the
   *  run's inbox and hands the model a one-line reference instead. Opt in
   *  only for a server that returns files that way: a long opaque token
   *  could otherwise look like one. Part of the registration's content, so
   *  changing it mints a new version. Sent as the overlay's `binary_results`. */
  binaryResults?: "passthrough" | "capture";
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

/** A hook event: a tool call's `pre` / `post` / `post_failure`, or a point in
 *  the run. */
export type HookEvent =
  | "pre" | "post" | "post_failure"
  | "agent_start" | "agent_stop" | "subagent_start" | "subagent_stop"
  | "pre_compact" | "post_compact" | "run_end"
  /** A channel's own event; an agent, a team or a run cannot attach it. */
  | "channel_publish";

/** A webhook written into the definition that attaches it. */
export interface InlineWebhook {
  name: string;
  url: string;
  fail_mode?: HookFailMode;
  timeout_ms?: number;
  /** Sent with each call. A value may name a credential as `$cred:<name>`,
   *  resolved for the run when the hook is called — keep secrets there, not in
   *  the URL or a literal value. */
  headers?: Record<string, string>;
}

/** One hook an agent carries: a HookDef name (`"gate"`, or `"gate@3"` pinned)
 *  or an inline webhook. */
export type HookEntry = string | InlineWebhook;

/** Hooks per event, in chain order. */
export type EventHooks = Partial<Record<HookEvent, HookEntry[]>>;

/** Each tool's own hooks, by tool name. */
export type ToolHooksByTool = Record<string, Partial<Record<"pre" | "post" | "post_failure", HookEntry[]>>>;

/** A `tools` entry that carries that tool's own hooks. */
export interface ToolEntry {
  name: string;
  hooks: Partial<Record<"pre" | "post" | "post_failure", HookEntry[]>>;
}

/** An agent's opt-in to the listwise search rerank (`memory_rerank`). A fork
 *  overlay merges it per field. */
export interface MemoryRerank {
  enabled?: boolean;
  /** How many search candidates the model is shown (2-50). Default 20. */
  candidates?: number;
  /** Per-candidate character budget (200-20000). Default 1200. */
  max_chars?: number;
}

/** An agent's narrowing of the server's decision models (`decision`). It can
 *  only narrow: every name must be one the server's `decision:` block lists. A
 *  fork overlay that sets it replaces the parent's block whole. */
export interface AgentDecision {
  /** The model that answers a Decision call naming none. One of `models` when
   *  `models` is set. */
  default?: string;
  /** The models a Decision call may name. Empty = every model the server lists. */
  models?: string[];
}

/** The `sampling` block of an {@link AgentDefOverlay}: {@link SamplingOptions}
 *  in its wire spelling. The overlay is sent verbatim, so its nested blocks are
 *  snake_case — the camelCase run-option types are converted by the client and
 *  would be ignored here. A fork merges it per field; an explicit
 *  `temperature: 0` is deterministic, not unset. */
export interface AgentSampling {
  temperature?: number;
  top_p?: number;
  top_k?: number;
  frequency_penalty?: number;
  presence_penalty?: number;
  seed?: number;
  stop?: string[];
}

/** The `compaction` block of an {@link AgentDefOverlay}: {@link CompactionOptions}
 *  in its wire spelling, plus `memory_flush`, which only an agent definition
 *  may set. A fork merges it per field. */
export interface AgentCompaction {
  enabled?: boolean;
  target_percentage?: number;
  keep_last_n?: number;
  keep_first?: boolean;
  autocompact_at_pct?: number;
  model?: string;
  /** Bank the span a compaction discards for the memory consolidator.
   *  Per-agent only: a run cannot set it, because it makes the agent persist
   *  memory its definition must authorize. */
  memory_flush?: boolean;
}

/** An agent's OCR and speech-to-text settings. It can opt in to OCR and lower
 *  the operator's per-run caps; it can never raise them or pick an engine. */
export interface AgentTranscription {
  /** OCR for scans and images. Default `never`: OCR is billed per page. */
  ocr?: 'auto' | 'always' | 'never';
  /** Speech-to-text. Default `on` when the operator configured an engine. */
  stt?: 'on' | 'off';
  /** At most the operator's `transcription.ocr.max_pages_per_run`. */
  max_pages_per_run?: number;
  /** At most the operator's `transcription.stt.max_minutes_per_run`. */
  max_minutes_per_run?: number;
}

/** An agent's text-to-speech settings. It can switch speech off for the agent
 *  and lower the operator's per-run character cap; it can never raise it or
 *  pick an engine, a model or a voice list. */
export interface AgentSpeech {
  /** Whether the agent may speak. Default `true` when the operator configured
   *  an engine; `false` switches it off for this agent. */
  enabled?: boolean;
  /** At most the operator's `speech.max_chars_per_run`. */
  max_chars_per_run?: number;
}

/** The `context` block of an {@link AgentDefOverlay}: {@link ContextOptions}
 *  in its wire spelling — see there for what each field does. A fork merges it
 *  per field, and the spawn tree inherits it. */
export interface AgentContext {
  mode?: ContextOptions["mode"];
  keep_last_n?: number;
  reasoning?: ContextOptions["reasoning"];
  recap_max_chars?: number;
  model?: string;
  autorecap_at_pct?: number;
  state_schema?: Record<string, unknown>;
  on_invalid_patch?: ContextOptions["onInvalidPatch"];
  max_patch_retries?: number;
  recall?: boolean;
  harvest_to_memory?: boolean;
}

/** One entry of a per-tier candidate list (`models` on an
 *  {@link AgentDefOverlay}). */
export interface TierCandidate {
  provider: string;
  model: string;
}

/** A core memory block an agent carries (`core_blocks`). Its value is the
 *  Memory entry `core/<label>` in the block's scope, rendered into the system
 *  prompt. */
export interface CoreBlock {
  /** One path segment: no `/`, no whitespace. */
  label: string;
  /** Where the value lives. An `agent` block never crosses a spawn boundary. */
  scope?: "agent" | "user" | "tenant";
  /** Caps an agent write to the block, in bytes of the JSON value. 0 = only the
   *  scope's `memory_quota_bytes`. */
  limit_bytes?: number;
  /** Operator-authored: the agent's writes to it are refused. */
  read_only?: boolean;
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
/** An agent's `routing:` block (RFC EI P4). */
export interface AgentRouting {
  /** Pass over a cascade candidate whose provider is busy enough that the
   *  run's first call would wait longer than `switch_after_ms`, for the next
   *  available one. */
  queue_aware?: boolean;
  /** The estimated wait that passes a candidate over. Default 20000. */
  switch_after_ms?: number;
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
  /** How the provider and model are chosen when a run starts (RFC EI P4).
   *  Tier-routed agents only. Replaced whole by a fork that sets it. */
  routing?: AgentRouting;
  /** Whether and which tool the model must call (RFC DI). */
  tool_choice?: ToolChoiceOptions;
  /** The JSON schema the answer must follow (RFC DI). */
  output_format?: OutputFormatOptions;
  max_tokens?: number;
  max_iterations?: number;
  max_concurrent_children?: number;
  system_prompt?: string;
  /** Tool names; an entry may be `{name, hooks}` to give that tool its own
   *  pre / post hooks (stored as the name plus a `tool_hooks` entry). */
  tools?: (string | ToolEntry)[];
  /** The agent's own hooks: run events, and tool events for every tool. A run
   *  takes these verbatim. */
  hooks?: EventHooks;
  /** Each tool's own hooks, by tool name. */
  tool_hooks?: ToolHooksByTool;
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
  /** Rerank this agent's document searches: `Memory op=search`, `Document
   *  op=search`, and `recall` when it asks for documents, reorder their first
   *  candidates with the operator's
   *  `memory.reranker` model. Costs one model call per search (about 6,000 prompt
   *  tokens), which is why it is set here and has NO tool parameter. The response
   *  reports `reranked` (and `rerank_reason` when false); a failed or unavailable
   *  rerank keeps the search's own order. */
  memory_rerank?: MemoryRerank;
  /** Which of the server's decision models this agent's `Decision` tool may ask,
   *  and its default. Unset: all of them, and the server's default. */
  decision?: AgentDecision;
  /** `false` makes this agent's searches ignore Document derived search units — the
   *  short descriptions, claims and questions an operator may generate for a
   *  document so a question can match them. Unset uses them wherever they exist
   *  (only where an operator generated them). A chunk found through a unit carries
   *  `matched_unit: {kind, text}`. */
  memory_units?: boolean;
  /** `false` locks the agent's per-run overrides for every caller: a run
   *  request that sets one (model, effort, limits, sampling, hooks, …) is
   *  refused with `override_not_allowed`. Unset or `true` leaves them to the
   *  caller. A fork that does not send it keeps its parent's. */
  allow_overrides?: boolean;
  /** `"ro"` gives the agent every ephemeral volume of its run read-only,
   *  whatever mode the volume was created with, and the same to every agent
   *  it spawns. The only value; a fork cannot clear it. */
  ephemeral_volumes?: "ro";
  /** `"none"` makes every run of the agent a no-trace run: its content is
   *  delivered to the caller and stored nowhere. A fork that does not send it
   *  keeps its parent's, and `"default"` on a no-trace parent is refused. */
  retention?: "none" | "default";
  memory_quota_bytes?: number;
  memory_backend?: string;
  retry_attempts?: number;
  /** Evaluation tool scope gate, e.g. `["submit_self", "read_any"]`. The
   *  Evaluation tool default-denies until granted. */
  evaluation_scopes?: string[];
  /** Crypto tool key scopes the agent's key-backed ops may use: `agent` |
   *  `user` | `tenant` | `run`. Unset (or `["-*"]`) grants none — keyless ops
   *  only. Held to the author's own grant; a sub-agent holds only what its
   *  parent run also holds. */
  crypto_scopes?: string[];
  /** Which operator-listed credentials the agent's `Bash` and Bashbox host
   *  commands receive (RFC EW): only these names of
   *  `LOOMCYCLE_BASH_ALLOWED_CREDS` / `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS`.
   *  Unset = every credential the operator lists; `[]` = none; never adds one.
   *  Held to the author's own narrowing; a sub-agent gets only the names its
   *  parent run also allows. */
  shell_credentials?: string[];
  /** Which network operations the agent's `Git` tool may perform (RFC FB):
   *  `['fetch']` (clone, fetch, pull, ls_remote) or `['fetch', 'push']`.
   *  Unset or `[]` = none, local repositories only; `push` needs `fetch`.
   *  Held to the author's own grant; a sub-agent may perform an operation
   *  only when its parent run also may. */
  git_remote?: Array<'fetch' | 'push'>;
  /** Which methods the agent may stamp on the document relations it writes:
   *  `'rule'` (its code found it) and/or `'extractor'` (a model it drives
   *  found it). Only for `provider: 'code-js'` — refused on any other
   *  provider. Unset or `[]` = none: every relation it writes is stamped
   *  `manual`. A new agent may list only what its author may stamp; a fork
   *  only what the version it forks held. */
  statement_methods?: Array<'rule' | 'extractor'>;
  /** Channel tool ACL (default-deny until set). */
  channels?: AgentChannelACL;
  /** Interruption tool gate — `enabled: true` REQUIRED for the tool to work. */
  interruption?: AgentInterruptionACL;
  /** The agent's own description — what an agent file's frontmatter
   *  carries — and part of its content hash. Not the request's top-level
   *  `description`, which is the rationale for this create/fork. */
  description?: string;
  /** Tier-routing override: the providers this agent's tier resolution tries,
   *  in order, replacing the server's priority list. No effect on a
   *  provider+model pin. */
  providers?: string[];
  /** Per-tier candidate lists, keyed by tier name. A tier set here replaces
   *  the server's definition of that tier for this agent. */
  models?: Record<string, TierCandidate[]>;
  /** The web-search providers the WebSearch tool tries, in order. Empty uses
   *  the server's default order. */
  search_providers?: string[];
  /** The context window this agent uses, in tokens — distinct from
   *  `max_tokens`, the output cap. Sizes the window on a local model; on a
   *  hosted one it can only lower the effective window. */
  max_context_tokens?: number;
  /** Lift the `max_iterations` cap for an LLM agent (cancel is the stop). A
   *  fork can set it but cannot clear it. */
  unbounded_iterations?: boolean;
  /** Wall-clock budget, in seconds, for a code-js agent; 0 = the server's
   *  default. Ignored by LLM agents. */
  run_timeout_seconds?: number;
  /** The system prompt before skill bodies are baked in. Defaults to
   *  `system_prompt`; set it only to rebuild a prompt from a different base. */
  system_prompt_base?: string;
  /** Per-agent LLM sampling, merged per field over the parent's. */
  sampling?: AgentSampling;
  /** Context compaction in `append` mode, merged per field. */
  compaction?: AgentCompaction;
  /** OCR and speech-to-text settings, merged per field. */
  transcription?: AgentTranscription;
  /** Text-to-speech settings, merged per field. */
  speech?: AgentSpeech;
  /** How history is distilled, merged per field. */
  context?: AgentContext;
  /** Volumes the file and exec tools may resolve paths against. None declared
   *  means `default`; declaring any confines the agent to exactly those. */
  volumes?: string[];
  /** SQL Memory scopes: `agent` | `user` | `run`. Unset means `["user"]`;
   *  `["-*"]` grants none. */
  sql_scopes?: string[];
  /** Per-scope SQL database byte cap; 0 = the server's default. */
  sql_quota_bytes?: number;
  /** Whose past chats the History tool may read: `self` | `user` | `tenant` |
   *  `global` (honored only for an admin). Unset means `["user"]`; `["-*"]`
   *  grants none. */
  history_scope?: string[];
  /** Core memory blocks rendered into the system prompt. */
  core_blocks?: CoreBlock[];
  /** A sub-agent also receives its parent run's user- and tenant-scope core
   *  blocks. */
  inherit_core_blocks?: boolean;
  /** Cap on all memory-injected prompt content, in tokens (estimated); 0 = the
   *  server's default. */
  memory_inject_max_tokens?: number;
  /** Opt into the memory-usage protocol note. */
  memory_protocol?: boolean;
  /** Soft size cap, in bytes, the memory protocol asks the agent to keep its
   *  index document under; 0 = the server's default. */
  memory_index_max_bytes?: number;
  /** Grant the Memory tool's consolidation control ops. Default-deny. */
  memory_consolidation?: boolean;
  /** Provisioning of the user-root memory document: `lazy` (default),
   *  `force` or `suppress`. */
  memory_roots?: "lazy" | "force" | "suppress";
  /** Append the runtime's capability and tool-guide text to the system prompt
   *  — for small models that under-read tool schemas. */
  inject_tool_guide?: boolean;
  /** Mark the agent as maintenance plumbing: its chats are excluded from
   *  memory consolidation and hidden from History by default. A fork can set
   *  it and cannot clear it. */
  internal?: boolean;
  /** Which AgentDef names this agent may fork / promote / retire: `self`,
   *  `descendants`, `named:<name>` or `any`. Default-deny. */
  agent_def_scopes?: string[];
  /** Same closed set as `agent_def_scopes`, for ScheduleDef. Default-deny. */
  schedule_def_scopes?: string[];
  /** Same closed set as `agent_def_scopes`, for A2AServerCardDef. Default-deny. */
  a2a_server_card_def_scopes?: string[];
  /** Same closed set as `agent_def_scopes`, for A2AAgentDef. Default-deny. */
  a2a_agent_def_scopes?: string[];
  /** Which volumes this agent may create / delete / purge: `named:<volume>`
   *  or `any`. Default-deny. */
  volume_def_scopes?: string[];
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

// ---- Typed HookDef / ScheduleDef / WebhookDef inputs (RFC EO) ----
//
// The overlays below mirror the Go shapes the server decodes them into
// (hooks.Def, mergedScheduleDef, mergedWebhookDef). A Go test
// (internal/tools/builtin/def_overlay_ts_parity_test.go) reflects over those
// structs and fails when a field is added on either side without the other.
//
// hookDef / scheduleDef / webhookDef accept these OR the loose
// SubstrateToolInput, so existing callers compile unchanged — and a bare
// object literal with a misspelt key still matches the loose shape there. The
// create / fork methods (createHookDef, forkHookDef, createScheduleDef, …) and
// ensureHookDef take ONLY the typed overlay, so a bare literal is checked.

/** A HookDef's body: a code-js script the server runs in-process, or a webhook
 *  it calls. The server refuses a code-js body carrying a url or headers, and
 *  an http body carrying code. */
export type HookDefBody =
  | {
      kind: "code-js";
      /** A script defining `function hook(ev)` that returns the decision. */
      code: string;
    }
  | {
      kind: "http";
      /** http:// or https://. */
      url: string;
      /** Sent with each call. A value may name a credential as
       *  `$cred:<name>`, resolved for the run when the hook is called — keep
       *  secrets there, not in the URL or a literal value. */
      headers?: Record<string, string>;
      /** Signs every delivery: the credential, as `"$cred:<name>"` and nothing
       *  else, whose value keys the `X-Loomcycle-Signature` HMAC. It must be
       *  usable for this `url` (unrestricted, or its `allowed_urls` covering
       *  it). A secret that cannot be resolved fails the call; it is never
       *  sent unsigned. Verify with `verifyLoomcycleDelivery`. */
      signing_secret?: string;
    };

/** Narrows a tool-event hook (`pre` / `post` / `post_failure`) within what it
 *  is attached to. The server refuses it on any other event. */
export type HookDefMatch = {
  /** Exact tool names or trailing-`*` globs; empty = every tool. */
  tools?: string[];
};

/** A HookDef's definition — the `overlay` of a `hookDef` create.
 *
 *  CLOSED on purpose (no index signature): the server decodes the overlay
 *  strictly and refuses any key it does not know, so an unknown key here is a
 *  compile error rather than a runtime refusal. A type alias rather than an
 *  interface so it stays assignable to the loose `SubstrateToolInput.overlay`. */
export type HookDefOverlay = {
  /** Shown to whoever the hook denies, holds or rewrites — including a tenant
   *  that did not write it. The body is never shown. At most 4 KiB. */
  description?: string;
  /** What the hook answers. Wherever it is attached, it is attached under this
   *  event; `channel_publish` is a channel's own. */
  event: HookEvent;
  match?: HookDefMatch;
  body: HookDefBody;
  /** `"open"` (default): a failing hook lets the call through. `"closed"`: it
   *  stops it. */
  fail_mode?: HookFailMode;
  /** Webhook: the whole call (default 5 s, max 60 s). code-js: each run of the
   *  script (default 50 ms, max 1 s). */
  timeout_ms?: number;
};

/** The `overlay` of a `hookDef` fork: each top-level field given replaces the
 *  parent's, and `null` clears one (it decodes to the field's zero value). */
export type HookDefForkOverlay = {
  [K in keyof HookDefOverlay]?: HookDefOverlay[K] | null;
};

/** Typed input for {@link LoomcycleClient.hookDef}, one shape per op. */
export type HookDefInput =
  | {
      op: "create";
      name: string;
      overlay: HookDefOverlay;
      /** Free-text rationale (distinct from `overlay.description`). */
      description?: string;
      /** Default true. */
      promote?: boolean;
    }
  | {
      op: "fork";
      name: string;
      /** Default: the active version of `name` in your tenant. */
      parent_def_id?: string;
      overlay?: HookDefForkOverlay;
      description?: string;
      /** Default false. */
      promote?: boolean;
    }
  /** By `def_id`, or the active version of `name`. */
  | { op: "get"; name?: string; def_id?: string }
  | { op: "list"; name: string }
  | { op: "promote"; def_id: string }
  | { op: "retire"; def_id: string; retired: boolean }
  | { op: "verify"; name: string; content_sha256?: string }
  | { op: "delete"; name: string };

/** One HookDef version, as create / fork / get return it (`list` returns
 *  `{name, versions: HookDefRowResponse[]}`). */
export interface HookDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  /** "sha256:" + 64 hex chars, over the name and the normalized definition. */
  content_sha256: string;
  tenant_id?: string;
  /** The stored definition, normalized: `fail_mode` filled in, empty fields
   *  left out. */
  definition: HookDefOverlay;
  /** On create / fork: whether the new version was made active. */
  promoted?: boolean;
}

/** Response shape for `HookDef verify`; same semantics as
 *  {@link MCPServerDefVerifyResult}. Asking without `content_sha256` is how to
 *  learn whether `name` is deployed at all: `deployed: false`, never an error. */
export interface HookDefVerifyResult {
  matches: boolean;
  current_sha256: string;
  current_def_id: string;
  version: number;
  name: string;
  deployed: boolean;
}

/** Options for the strictly-typed create methods
 *  ({@link LoomcycleClient.createHookDef},
 *  {@link LoomcycleClient.createScheduleDef},
 *  {@link LoomcycleClient.createWebhookDef}). */
export interface CreateDefOptions {
  /** Free-text rationale stored on the version (not part of the definition). */
  description?: string;
  /** Make the new version active. Server default: true. */
  promote?: boolean;
  signal?: AbortSignal;
}

/** Options for the strictly-typed fork methods. */
export interface ForkDefOptions extends CreateDefOptions {
  /** The version to fork. Default: the active version of the name in your
   *  tenant (ScheduleDef / WebhookDef fall back to the yaml template of that
   *  name). Server default for `promote` on a fork: false for HookDef, true
   *  for ScheduleDef and WebhookDef. */
  parentDefId?: string;
}

/** Options for {@link LoomcycleClient.ensureHookDef}. */
export interface EnsureHookDefOptions {
  /** Free-text rationale stored on a NEW version. Not part of the definition,
   *  so changing only this does not mint one. */
  description?: string;
  signal?: AbortSignal;
}

/** Result of {@link LoomcycleClient.ensureHookDef}. */
export interface EnsureHookDefResult {
  name: string;
  defId: string;
  version: number;
  /** True when this call created a version; false when the active version
   *  already carried the same definition. */
  changed: boolean;
}

/** One `on_complete` entry of a ScheduleDef or WebhookDef: what the server
 *  does after the triggered run finishes. Mirrors config.ScheduledRunHook. */
export type ScheduledRunHook =
  | {
      kind: "channel.publish";
      channel: string;
      payload?: Record<string, unknown>;
    }
  | {
      kind: "mcp.call";
      server: string;
      tool: string;
      args?: Record<string, unknown>;
    }
  | {
      kind: "memory.set";
      scope: string;
      key: string;
      /** The value written. */
      payload?: Record<string, unknown>;
    };

/** One block of a scheduled run's prompt. Only text survives into the run, so
 *  there is no image form here. */
export interface ScheduledRunPromptContent {
  /** Default `"trusted-text"`. `"untrusted-block"` wraps the text as external
   *  input. */
  type?: "trusted-text" | "untrusted-block";
  text: string;
  /** An untrusted-block's source label, as on a run's segments; `"code"`
   *  keeps the text exactly as written. Default `"untrusted"`. */
  kind?: string;
}

/** One segment of a scheduled run's prompt. */
export interface ScheduledRunPromptSegment {
  role: "system" | "user";
  content: ScheduledRunPromptContent[];
}

/** The `overlay` of a `scheduleDef` create / fork. On fork, a field the
 *  overlay leaves out keeps the parent's; the credential maps merge key by key.
 *
 *  The `[extra]` tail stays because the server decodes the overlay loosely and
 *  ignores keys it does not know — including the fields it stamps itself
 *  (`operator_key_restricted`, `isolated`, `capture_disabled`,
 *  `operator_layer`), which an overlay can neither set nor clear. */
export interface ScheduleDefOverlay {
  /** `"run"` (default) invokes `agent`; `"channel"` publishes a tick to
   *  `channel` and starts no run; `"team"` starts a detached walk of `team`. */
  delivery?: "run" | "channel" | "team";
  channel?: string;
  agent?: string;
  prompt?: ScheduledRunPromptSegment[];
  /** A 5-field cron expression. Setting it clears `user_tier_schedules`. */
  schedule?: string;
  /** Cron per user tier, for a template a fork picks from with `user_tier`.
   *  Setting it clears `schedule`. */
  user_tier_schedules?: Record<string, string>;
  required_credentials?: string[];
  /** IANA zone the cron is read in, e.g. `"Europe/Berlin"`. Default UTC. */
  timezone?: string;
  enabled?: boolean;
  /** 0–1000: run the newest N slots missed during an outage, in slot order.
   *  0 (default) collapses them into one fire. */
  catch_up_max?: number;
  /** N > 0 retires the def after its Nth fire; 0 = unbounded (a fork sends 0
   *  to lift a parent's cap). */
  max_fires?: number;
  user_id?: string;
  /** What a slot does while the previous run is still going. Default
   *  `"forbid"`; refused on `delivery: "channel"`. */
  concurrency_policy?: "forbid" | "allow" | "replace";
  user_tier?: string;
  /** Credential key → value, handed to the fired run. */
  user_credentials?: Record<string, string>;
  /** Credential key → env-var NAME the scheduler reads it from (the variable
   *  must be on LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST). */
  user_credentials_from_env?: Record<string, string>;
  /** The fired run's bearer — what `${run.user_bearer}` in an MCP server's
   *  headers becomes. Only a stored credential, `"$cred:<name>"`: resolved
   *  for each MCP request, for that server's URL, as a tenant-level
   *  credential of the run's tenant (never a user's or an agent's), so a
   *  credential restricted by `allowed_urls` works. Refused on delivery
   *  `channel` or `team`. */
  user_bearer?: string;
  on_complete?: ScheduledRunHook[];
  /** Non-secret metadata the agent receives as trusted. */
  metadata?: Record<string, unknown>;
  /** The tenant fired runs execute in. Your own unless you are an admin;
   *  omit it to use yours. */
  tenant_id?: string;
  /** `delivery: "team"` only: the team, its variables as literals, and the
   *  walk's optional input. */
  team?: string;
  vars?: Record<string, string>;
  input?: string;
  [extra: string]: unknown;
}

/** Typed input for {@link LoomcycleClient.scheduleDef}, one shape per op. */
export type ScheduleDefInput =
  | {
      op: "create" | "fork";
      name: string;
      /** fork only. Default: the active version of `name`, or its yaml
       *  template. */
      parent_def_id?: string;
      overlay?: ScheduleDefOverlay;
      description?: string;
      /** Default true on both create and fork. */
      promote?: boolean;
    }
  | { op: "get"; def_id: string }
  | { op: "list"; name: string }
  | { op: "retire"; def_id: string; retired: boolean }
  /** Fork `def_id` with `hook` appended to its `on_complete`. */
  | { op: "add_hook"; def_id: string; hook: ScheduledRunHook }
  /** Fork `def_id` without its `on_complete[hook_index]` (0-based). */
  | { op: "remove_hook"; def_id: string; hook_index: number };

/** One ScheduleDef version, as create / fork return it. */
export interface ScheduleDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  /** The stored definition, including the fields the server stamps. */
  definition: ScheduleDefOverlay;
  /** Whether the new version was made active. */
  promoted?: boolean;
  /** Credential keys a snapshot stripped that this write did not re-supply;
   *  the def stays disabled until they are. */
  disabled_until_credentials_supplied?: string[];
  /** Why a key you did name did not count, one line per key. */
  unusable_credentials?: string[];
}

/** How a WebhookDef authenticates an inbound request. */
export interface WebhookDefAuth {
  /** `"hmac"` (default) verifies a body signature; `"bearer"` a token;
   *  `"none"` neither. */
  kind?: "hmac" | "bearer" | "none";
  /** hmac only; the only one implemented. */
  algorithm?: "sha256";
  /** The header carrying the signature. */
  header?: string;
  /** Env-var NAME holding the hmac secret — never the secret itself. */
  signing_secret_env?: string;
  /** The header naming each delivery, for dedup; without it a body hash is
   *  the delivery's identity. */
  delivery_id_header?: string;
  /** Env-var NAME holding the bearer token — never the token itself. */
  bearer_token_env?: string;
  /** The verification secret as a stored credential, `"$cred:<name>"` — a
   *  tenant credential of the definition's own tenant, resolved on each
   *  delivery so a rotation needs no restart. Takes the place of
   *  `signing_secret_env` / `bearer_token_env`: set one form, not both. A
   *  literal is refused. */
  secret?: string;
  /** The previous secret during a rotation, also `"$cred:<name>"`: a delivery
   *  verified by either is accepted. */
  previous_secret?: string;
}

/** One condition a webhook delivery's signed body must meet. Comparison is
 *  typed (a string never equals a number), and a path that is missing, null,
 *  or of another type than the value misses for every `op`, `ne` included. */
export interface WebhookDefFilter {
  /** A JSONPath into the body: `$`, `.key` and `[N]` only. */
  path: string;
  /** `glob` takes a string pattern (`*` any run of characters, `?` one);
   *  `in` takes a list of values. */
  op: "eq" | "ne" | "glob" | "in";
  /** A literal; set this or `value_from`. */
  value?: string | number | boolean | Array<string | number | boolean>;
  /** `"metadata.<key>"`: the definition's own metadata entry of that key. */
  value_from?: string;
}

/** Inbound request rate bound for one webhook. */
export interface WebhookDefRateLimit {
  requests_per_minute?: number;
  burst?: number;
}

/** Hold the inbound request open until the run finishes (or the timeout). */
export interface WebhookDefSyncResponse {
  enabled?: boolean;
  timeout_ms?: number;
}

/** The `overlay` of a `webhookDef` create / fork. On fork, a field the overlay
 *  leaves out keeps the parent's; naming exactly one of `agent` / `channel` /
 *  `team` clears the other two.
 *
 *  The `[extra]` tail stays for now: the server decodes the overlay loosely
 *  and ignores keys it does not know — including the fields it stamps itself
 *  (`operator_key_restricted`, `isolated`, `capture_disabled`). */
export interface WebhookDefOverlay {
  /** An overlay can only turn a webhook on; to turn one off, restate it. */
  enabled?: boolean;
  /** `"spawn"` (default) starts a run of `agent`; `"channel"` publishes the
   *  body to `channel`; `"team"` starts a detached walk of `team` with the body
   *  as its input. */
  delivery?: "spawn" | "channel" | "team";
  agent?: string;
  channel?: string;
  auth?: WebhookDefAuth;
  rate_limit?: WebhookDefRateLimit;
  body_size_limit_bytes?: number;
  /** Credential key → env-var NAME the receiver reads it from. */
  user_credentials_from_env?: Record<string, string>;
  /** Credential key → value, handed to the run. */
  user_credentials?: Record<string, string>;
  /** The started run's bearer — what `${run.user_bearer}` in an MCP server's
   *  headers becomes. Only a stored credential, `"$cred:<name>"`: resolved
   *  for each MCP request, for that server's URL, as a tenant-level
   *  credential of the run's tenant (never a user's or an agent's), so a
   *  credential restricted by `allowed_urls` works. Refused on delivery
   *  `channel` or `team`. */
  user_bearer?: string;
  /** Pins the started run's user tier; a delivery never chooses it. Must be
   *  one of the server's user tiers. */
  user_tier?: string;
  /** A forge preset — `"github"`, `"gitea"` or `"gitlab"` — supplying
   *  defaults for the auth kind and headers, the event header, the ping and
   *  the shapes of the events it knows. A field set here overrides it. */
  provider?: string;
  /** The request header naming a delivery's event. */
  event_header?: string;
  /** When set, the only events that start anything: another is answered 202
   *  `ignored`, the provider's ping 200, and a listed event counts only when
   *  the signed body has its shape. Unset: every delivery is handled. */
  events?: string[];
  /** The rules the body of a listed event must meet, for an event the
   *  provider has no shape for, or in place of its. Each rule is a JSONPath
   *  the body holds, `!path` for one it must not hold (missing or null), or
   *  `path == <JSON string, number or boolean>` for a field's value, e.g.
   *  `$.object_kind == "push"`. A fork merges key by key; an empty list
   *  removes that event's shape. */
  event_shapes?: Record<string, string[]>;
  /** Conditions on the signed body every delivery must meet before anything
   *  starts; one that misses answers 202 `filtered`. A fork replaces the
   *  whole list; `[]` removes it. */
  filters?: WebhookDefFilter[];
  /** JSONPaths into the signed body whose values together name one event
   *  (for a GitHub push: `$.repository.id`, `$.ref`, `$.after`). A delivery
   *  with the same values is answered as a duplicate whatever its delivery id
   *  or bytes, unless the run the first one started ended `failed`. A body
   *  lacking one answers 400 `dedup_key_unresolvable`. 1 to 16 paths; a fork
   *  replaces the list, `[]` removes it. */
  dedup_key?: string[];
  /** How long a `dedup_key` is held, in seconds: default 604800 (7 days),
   *  at most 90 days. Only beside `dedup_key`. */
  dedup_ttl_seconds?: number;
  /** Non-secret metadata the agent receives as trusted. */
  metadata?: Record<string, unknown>;
  /** The tenant spawned runs execute in — taken from the def, never from the
   *  payload. Your own unless you are an admin; omit it to use yours. */
  tenant_id?: string;
  /** Target → JSONPath into the request body (`$`, `.key` and `[N]` only).
   *  Targets: `goal`, `user_id`, `user_credentials.<KEY>`,
   *  `run_metadata.<key>`. */
  payload_mapping?: Record<string, string>;
  sync_response?: WebhookDefSyncResponse;
  on_complete?: ScheduledRunHook[];
  /** `delivery: "team"` only: the team, and each variable as a JSONPath into
   *  the body (`$`, `.key` and `[N]` only). */
  team?: string;
  vars?: Record<string, string>;
  [extra: string]: unknown;
}

/** One WebhookDef version, as create / fork return it. */
export interface WebhookDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  /** The stored definition, including the fields the server stamps. */
  definition: WebhookDefOverlay;
  promoted?: boolean;
  /** Same meaning as on {@link ScheduleDefRowResponse}. */
  disabled_until_credentials_supplied?: string[];
  unusable_credentials?: string[];
  /** `get` only: how the webhook's deliveries were answered. */
  status?: { deliveries?: WebhookDefDeliveries };
}

/** A webhook's delivery counts, on `webhookDef({op: "get"})` as
 *  `status.deliveries`. They belong to the webhook (its name in its tenant),
 *  not to one version; every replica adds its counts every 30 seconds, so
 *  they are up to that far behind. */
export interface WebhookDefDeliveries {
  accepted: number;
  duplicate: number;
  ping: number;
  ignored: number;
  filtered: number;
  /** Refused deliveries by reason: `signature`, `secret`, `event`, `shape`,
   *  `body`, `mapping`, `rate`, `dedup_key`, … */
  refused: Record<string, number>;
  /** Deliveries that ended otherwise (a sync `timeout`, `client_gone`). */
  other?: Record<string, number>;
  /** When a counted delivery was last written (RFC 3339). */
  last_at?: string;
}

/** Typed input for {@link LoomcycleClient.webhookDef}, one shape per op. */
export type WebhookDefInput =
  | {
      op: "create" | "fork";
      name: string;
      /** fork only. Default: the active version of `name`, or its yaml
       *  template. */
      parent_def_id?: string;
      overlay?: WebhookDefOverlay;
      description?: string;
      /** Default true on both create and fork. */
      promote?: boolean;
    }
  | { op: "get"; def_id: string }
  | { op: "list"; name: string }
  | { op: "retire"; def_id: string; retired: boolean };

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
 *  inspector when ready). Secret-bearing values come back masked as
 *  "****": every header value (an auth scheme is kept, "Bearer ****"),
 *  every env value, and the url's userinfo, query values and fragment. */
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
  /** /v1/_library/agents only: the context window, in tokens, a run of
   *  this agent starts with — the resolved provider's window, lowered by
   *  the agent's own max_context_tokens. Omitted when unknown. */
  context_window?: number;
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
  | "source"
  /** What the calls were for (see `UsageAggregate.purpose`). */
  | "purpose"
  /** The model tier the calls were resolved through; "pinned" for a named model. */
  | "tier"
  | "user_tier"
  /** The agent that made the calls — the per-call detail window only. */
  | "agent"
  /** foreground | background, derived from the purpose. */
  | "class";

/** The period a usage report splits its window into (UTC; weeks start Monday). */
export type UsageBucket = "hour" | "day" | "week" | "month";

/** One grouped row of a usage report; only the grouped dimensions are set. */
export interface UsageAggregate {
  tenant_id?: string;
  user_id?: string;
  provider?: string;
  model?: string;
  /** operator | tenant | user */
  credential_source?: string;
  /** chat | gateway | consolidation | rerank | decision | compaction | recap |
   *  unit_generation | image_description | embedding | ocr | stt | tts — set
   *  when grouped by `purpose`. */
  purpose?: string;
  tier?: string;
  user_tier?: string;
  agent?: string;
  /** foreground | background — set when grouped by `class`. */
  class?: string;
  /** RFC 3339 start of the UTC period the row sums, when a bucket was asked for. */
  period?: string;
  /** The row folds every value of the first dimension outside `top`. */
  other?: boolean;
  /** Characters text-to-speech and embedding calls sent; absent when none. */
  input_chars?: number;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  cost: number;
  currency?: string;
  call_count: number;
  unpriced_calls: number;
  /** Seconds of audio the group's speech calls transcribed (stt) or produced
   *  (tts), the unit a per-minute price is computed from; absent when it has none. */
  audio_seconds?: number;
}

export interface UsageReportResponse {
  group_by: string[];
  from?: string;
  to?: string;
  bucket?: UsageBucket;
  rows: UsageAggregate[];
  /** The oldest instant the per-call ledger holds; before it only whole days
   *  survive, so hour buckets and the agent dimension stop there. Absent when
   *  nothing is archived. */
  detail_since?: string;
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
  /** The https URLs the value may be sent to, in canonical form. Absent for
   *  an unrestricted credential; `[]` for one that is sent nowhere. */
  allowed_urls?: string[];
  /** The user name sent with the value where a destination takes one (HTTP
   *  basic auth for a git source). Absent when none is stored. */
  username?: string;
  /** Present with `expires_at`: true once that time has passed. An expired
   *  credential is kept so it can be seen and rotated, but resolves nowhere. */
  expired?: boolean;
  /** `"crypto"` for a key the Crypto tool uses (RFC EP); absent for an
   *  ordinary secret. The fields below are present only for a crypto key. */
  usage?: "crypto";
  /** The crypto key's type, fixed for its life. */
  key_type?: CryptoKeyType;
  /** The external KMS holding the key; absent for a key held by loomcycle.
   *  Such a key's versions are the KMS's, rotated and retired there. */
  key_backend?: CryptoKeyBackend;
  /** The version new encryptions and signatures use. */
  current_version?: number;
  /** The lowest version still used; every version below it is retired. */
  min_version?: number;
  /** Every stored version, never its material. */
  versions?: CryptoKeyVersion[];
}

/** One version of a crypto key: when it was made and whether it is retired
 *  (below `min_version` — nothing decrypts or verifies with it, and only a
 *  retired version can be deleted). */
export interface CryptoKeyVersion {
  version: number;
  /** RFC3339. */
  created_at: string;
  retired: boolean;
}

/** CredentialListResponse is the {op:list} result: metadata for the given scope. */
export interface CredentialListResponse {
  scope: string;
  credentials: CredentialMeta[];
}

/** What {@link LoomcycleClient.exportCryptoKey} returns: the key, every
 *  version of it, encrypted under the passphrase. Hand `blob` and the
 *  passphrase to {@link LoomcycleClient.importCryptoKey} on the other
 *  deployment. Anyone holding both holds the key. */
export interface CryptoKeyExport {
  name: string;
  scope: string;
  blob: string;
}

// ─── RFC EP Crypto tool (POST /v1/_crypto) ───────────────────────────────────

/** A crypto key's type. AEAD (`aes-256-gcm`, `xchacha20-poly1305`):
 *  encrypt / decrypt / rewrap. HMAC (`hmac-sha256`, `hmac-sha512`): mac /
 *  verify_mac, and sign / verify as a JWS. Signing (`ed25519`, `ecdsa-p256`,
 *  `rsa-pss-*`): sign / verify / public_key. Recipient (`x25519`,
 *  `rsa-oaep-*`): encrypt (a JWE to its public half) / decrypt / public_key.
 *  `totp`: totp. An RSA key either signs or decrypts, never both. */
export type CryptoKeyType =
  | "aes-256-gcm"
  | "xchacha20-poly1305"
  | "hmac-sha256"
  | "hmac-sha512"
  | "ed25519"
  | "ecdsa-p256"
  | "totp"
  | "x25519"
  | "rsa-oaep-2048"
  | "rsa-oaep-3072"
  | "rsa-oaep-4096"
  | "rsa-pss-2048"
  | "rsa-pss-3072"
  | "rsa-pss-4096";

/** An external KMS that can hold a crypto key. */
export type CryptoKeyBackend = "vault" | "aws_kms" | "gcp_kms";

/** Where a key held in an external KMS lives, for
 *  {@link LoomcycleClient.createKmsCryptoKey}. Each credential field is a
 *  `$cred:<name>` reference to an ordinary credential in the same scope as
 *  the key; loomcycle stores only the reference. */
export type CryptoKeyExternal =
  | {
      /** Vault Transit. The address's host must be on the operator's host
       *  allowlist. */
      backend: "vault";
      address: string;
      /** The Transit mount (default `transit`). */
      mount?: string;
      /** The Transit key name. */
      key: string;
      namespace?: string;
      /** `$cred:<name>` holding a Vault token. */
      token: string;
    }
  | {
      backend: "aws_kms";
      region: string;
      /** A key id, key ARN, `alias/<name>` or alias ARN. */
      key_id: string;
      access_key_id: string;
      secret_access_key: string;
      session_token?: string;
    }
  | {
      backend: "gcp_kms";
      /** `projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>`, optionally
       *  `/cryptoKeyVersions/<n>` (signing and RSA keys only). */
      key: string;
      /** `$cred:<name>` holding a service-account key JSON. */
      service_account: string;
    };

/** How key material handed in as a string is written. Default by key type:
 *  base64 for AEAD and HMAC, base32 for totp, pem for the asymmetric types. */
export type CryptoKeyEncoding = "raw" | "base64" | "hex" | "base32" | "pem";

/** The Crypto tool's operations. */
export type CryptoOp =
  | "encrypt"
  | "decrypt"
  | "rewrap"
  | "mac"
  | "verify_mac"
  | "sign"
  | "verify"
  | "public_key"
  | "totp"
  | "hash"
  | "random"
  | "hash_password"
  | "verify_password";

/** The fields every {@link CryptoRequest} may carry. */
export interface CryptoRequestBase {
  /** Admin only (or the legacy token, or open mode): run the op with another
   *  tenant's keys. It then reaches that tenant's keys and no user keys. A
   *  non-admin naming a tenant other than its own is refused. */
  tenant?: string;
}

/** Which key an op uses: exactly one of `key` (a crypto key stored with
 *  CredentialDef — type and versions come with it), `cred` (an ordinary
 *  stored credential used as key material — needs `key_type`) or `run` (a
 *  credential a run's caller passed; there is none outside a run, so the
 *  operator API refuses it). Outside a run the reachable stored keys are the
 *  token's tenant's and its own subject's user keys. */
export interface CryptoKeyRef {
  key?: string;
  cred?: string;
  run?: string;
  /** `key` and `cred`: which scope's key, when more than one reachable scope
   *  holds that name. An envelope already records it. */
  key_scope?: "agent" | "user" | "tenant";
  /** `cred` and `run`: what kind of key the value is. */
  key_type?: CryptoKeyType;
  /** `cred` and `run`: how the value is written. */
  key_encoding?: CryptoKeyEncoding;
  /** `cred` and `run`: derive an AEAD or HMAC key from any secret instead of
   *  reading the value as a key. */
  derive?: boolean;
  /** `key` only: a version, for public_key or a raw ciphertext. Default: the
   *  current one (decrypt of a raw ciphertext tries every usable version). */
  version?: number;
}

export interface CryptoEncryptRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "encrypt";
  /** Text, or base64 bytes with `input_encoding: "base64"`. */
  plaintext: string;
  input_encoding?: "text" | "base64";
  /** Associated data: the ciphertext decrypts only with the same context.
   *  Not with a JWE, which carries none. */
  context?: string;
  /** `envelope` (default for symmetric keys, names its key), `raw` (base64
   *  nonce+ct+tag) or `jwe` (aes-256-gcm; the only form for an x25519 or
   *  rsa-oaep key and for `public_key`). */
  format?: "envelope" | "raw" | "jwe";
  /** A PEM or JWK X25519 or RSA public key to encrypt to, in place of a held
   *  key: the result is a JWE only that key's holder can open. */
  public_key?: string;
}

export interface CryptoDecryptRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "decrypt";
  /** Exactly as encrypt returned it: an envelope, raw base64 or a JWE. An
   *  envelope from a crypto key names its key, so `key` / `cred` / `run` may
   *  be left out. */
  ciphertext: string;
  context?: string;
  /** Keeps the plaintext out of a run's stored transcript. */
  sensitive?: boolean;
  /** `text` (default when the plaintext is UTF-8) or `base64`. */
  output_encoding?: "text" | "base64";
}

export interface CryptoRewrapRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "rewrap";
  /** A crypto key's ciphertext; `key` is needed only for a raw one. */
  ciphertext: string;
  context?: string;
}

export interface CryptoMacRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "mac";
  data: string;
  input_encoding?: "text" | "base64";
  /** `hex` (default) or `base64`. */
  output_encoding?: "hex" | "base64";
}

export interface CryptoVerifyMacRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "verify_mac";
  data: string;
  /** The MAC to check, hex or base64, with any `sha256=` prefix stripped. */
  mac: string;
  input_encoding?: "text" | "base64";
}

export interface CryptoSignRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "sign";
  /** What to sign; for a JWS, the token's payload. */
  data: string;
  input_encoding?: "text" | "base64";
  /** `raw` (default for signing keys) or `jws` (the only form for a MAC key). */
  format?: "raw" | "jws";
}

export interface CryptoVerifyRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "verify";
  /** A raw base64 signature, or a JWS token (then leave `data` out). */
  signature: string;
  data?: string;
  input_encoding?: "text" | "base64";
  /** A PEM or JWK Ed25519, P-256 or RSA (PS256) public key, in place of a
   *  held key. */
  public_key?: string;
}

export interface CryptoPublicKeyRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "public_key";
}

export interface CryptoTotpRequest extends CryptoRequestBase, CryptoKeyRef {
  op: "totp";
  /** Code length, 6 to 8 (default 6). */
  digits?: number;
  /** Seconds per code (default 30). */
  period?: number;
}

/** A digest; needs no key. Never a password store. */
export interface CryptoHashRequest extends CryptoRequestBase {
  op: "hash";
  data: string;
  input_encoding?: "text" | "base64";
  /** Default `sha256`. */
  algorithm?: "sha256" | "sha512" | "sha3-256" | "sha3-512";
  /** `hex` (default) or `base64`. */
  output_encoding?: "hex" | "base64";
}

/** Secure random values; needs no key. */
export interface CryptoRandomRequest extends CryptoRequestBase {
  op: "random";
  /** `token` (default, URL-safe base64 without padding), `bytes` or `uuid`. */
  kind?: "token" | "bytes" | "uuid";
  /** Random bytes for `token` and `bytes`, 1 to 4096 (default 32). */
  length?: number;
  /** `bytes` only: `hex` (default) or `base64`. */
  output_encoding?: "hex" | "base64";
}

/** An Argon2id hash of a password to store; needs no key. */
export interface CryptoHashPasswordRequest extends CryptoRequestBase {
  op: "hash_password";
  /** Up to 1024 bytes. */
  password: string;
}

/** Checks a password against a hash from hash_password; needs no key. */
export interface CryptoVerifyPasswordRequest extends CryptoRequestBase {
  op: "verify_password";
  password: string;
  /** Exactly as hash_password returned it. */
  password_hash: string;
}

/** The body of {@link LoomcycleClient.crypto}, discriminated by `op`. */
export type CryptoRequest =
  | CryptoEncryptRequest
  | CryptoDecryptRequest
  | CryptoRewrapRequest
  | CryptoMacRequest
  | CryptoVerifyMacRequest
  | CryptoSignRequest
  | CryptoVerifyRequest
  | CryptoPublicKeyRequest
  | CryptoTotpRequest
  | CryptoHashRequest
  | CryptoRandomRequest
  | CryptoHashPasswordRequest
  | CryptoVerifyPasswordRequest;

/** `key_version` on a result is the crypto key version used; 0 for a `cred`
 *  or `run` key, which has no versions. */
export interface CryptoEncryptResult {
  /** An envelope (`lcx1:…`), base64 with `format: "raw"`, or a compact JWE. */
  ciphertext: string;
  /** `"jwe"` when the ciphertext is a JWE. */
  format?: "jwe";
  /** Absent when encrypting to a `public_key`. */
  key_version?: number;
}

export interface CryptoDecryptResult {
  plaintext: string;
  /** How `plaintext` is written. */
  encoding: "text" | "base64";
  key_version: number;
}

export interface CryptoRewrapResult {
  /** Store it in place of the old ciphertext. A JWE stays a JWE. */
  ciphertext: string;
  format?: "jwe";
  from_version: number;
  key_version: number;
}

export interface CryptoMacResult {
  mac: string;
  key_version: number;
}

/** verify_mac and verify. `valid: false` never says why and carries no
 *  `key_version`. */
export interface CryptoVerifyResult {
  valid: boolean;
  /** The held key's version that verified. */
  key_version?: number;
  /** verify of a JWS token: its `kid` header. */
  kid?: string;
  /** verify of a JWS token: its payload. */
  payload?: string;
}

export interface CryptoSignResult {
  signature: string;
  format: "raw" | "jws";
  key_version: number;
}

/** A public key as a JWK: `OKP`/`Ed25519` or `OKP`/`X25519` (x), `EC`/`P-256`
 *  (x, y) or `RSA` (n, e). The `kid` matches the `kid` in tokens the key signs
 *  or JWEs made to it; an RSA or X25519 key also names its `use` and `alg`. */
export interface CryptoPublicJWK {
  kty: "OKP" | "EC" | "RSA";
  crv?: "Ed25519" | "X25519" | "P-256";
  x?: string;
  y?: string;
  n?: string;
  e?: string;
  use?: "sig" | "enc";
  alg?: "PS256" | "RSA-OAEP-256" | "ECDH-ES";
  kid?: string;
}

export interface CryptoPublicKeyResult {
  pem: string;
  jwk: CryptoPublicJWK;
  key_version: number;
}

export interface CryptoTotpResult {
  code: string;
  /** RFC3339. */
  valid_until: string;
}

export interface CryptoHashResult {
  algorithm: string;
  digest: string;
}

export interface CryptoRandomResult {
  kind: "token" | "bytes" | "uuid";
  value: string;
}

export interface CryptoHashPasswordResult {
  /** A PHC string (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`), safe to store. */
  password_hash: string;
  algorithm: "argon2id";
}

export interface CryptoVerifyPasswordResult {
  valid: boolean;
}

/** The result type of each Crypto op. */
export interface CryptoResultByOp {
  encrypt: CryptoEncryptResult;
  decrypt: CryptoDecryptResult;
  rewrap: CryptoRewrapResult;
  mac: CryptoMacResult;
  verify_mac: CryptoVerifyResult;
  sign: CryptoSignResult;
  verify: CryptoVerifyResult;
  public_key: CryptoPublicKeyResult;
  totp: CryptoTotpResult;
  hash: CryptoHashResult;
  random: CryptoRandomResult;
  hash_password: CryptoHashPasswordResult;
  verify_password: CryptoVerifyPasswordResult;
}

/** Any Crypto op's result. */
export type CryptoResult = CryptoResultByOp[CryptoOp];

// ─── Decision models ─────────────────────────────────────────────────────────

/** A `choice` question: pick one of the options. Each key of `criteria` is an
 *  option and its value describes it, or is `null` when the key explains
 *  itself. The key is what comes back as the answer. */
export interface DecisionChoiceQuestion {
  type: "choice";
  /** The question itself, in plain words. */
  instructions: string;
  criteria: Record<string, string | null>;
}

/** A `noul` question: yes or no. `criteria` is optional: describe `true`
 *  and/or `false` when the plain question is not enough. */
export interface DecisionNoulQuestion {
  type: "noul";
  instructions: string;
  criteria?: { true?: string; false?: string };
}

/** A `score` question: a position on a scale. `criteria` is the level
 *  descriptions, LOWEST FIRST. */
export interface DecisionScoreQuestion {
  type: "score";
  instructions: string;
  criteria: string[];
}

export type DecisionQuestion =
  | DecisionChoiceQuestion
  | DecisionNoulQuestion
  | DecisionScoreQuestion;

/** Body of {@link LoomcycleClient.decide}: every question is answered about the
 *  same `state`. */
export interface DecideRequest {
  /** Which decision model answers: a name from
   *  {@link LoomcycleClient.listDecisionModels}. Omit for the default. */
  model?: string;
  /** What the questions are about. Any JSON object; the model reads only this,
   *  and it is never shortened for you (an over-long request is refused with
   *  code `prompt_too_large`). */
  state: Record<string, unknown>;
  /** The questions, keyed by names you choose; each answer comes back under its
   *  question's name. */
  questions: Record<string, DecisionQuestion>;
}

/** The answer to a `choice` question. */
export interface DecisionChoiceAnswer {
  type: "choice";
  /** The option picked: one of the question's `criteria` keys. */
  choice: string;
  /** One probability per option, summing to 1. */
  probabilities: Record<string, number>;
  /** How concentrated the probabilities are: near 1 when one option holds
   *  almost all the weight, near 0 when they are spread evenly. */
  confidence: number;
  /** The answer is the model's own, passed through unchanged; a later model may
   *  add fields. */
  [extra: string]: unknown;
}

/** The answer to a `noul` question. */
export interface DecisionNoulAnswer {
  type: "noul";
  /** The probability that the answer is YES, from 0 to 1. `0` is a definite
   *  no, not a missing answer: test it with `=== undefined`, never truthiness. */
  noul: number;
  [extra: string]: unknown;
}

/** The answer to a `score` question. */
export interface DecisionScoreAnswer {
  type: "score";
  /** The expected position on the scale, counted from 0 (`1.7` is between
   *  level 1 and level 2). */
  score: number;
  /** Position → the description you gave that level. */
  legend: Record<string, string>;
  /** One probability per position. */
  probabilities: Record<string, number>;
  confidence: number;
  [extra: string]: unknown;
}

/** One answer, discriminated on `type` (the type of the question it answers). */
export type DecisionAnswer =
  | DecisionChoiceAnswer
  | DecisionNoulAnswer
  | DecisionScoreAnswer;

/** Result of {@link LoomcycleClient.decide}. The probabilities are a judgement,
 *  not a measurement: compare options within one answer, and do not treat a
 *  fixed threshold as a guarantee. */
export interface DecideResponse {
  /** The name that answered, as the deployment lists it (the default's name
   *  when the request named none). */
  model: string;
  provider: string;
  /** What the provider says answered. */
  served_model: string;
  /** One answer per question, under the question's name. */
  answers: Record<string, DecisionAnswer>;
  /** Tokens of this call, charged to the caller and counted against its token
   *  budget. */
  usage: { input_tokens: number; output_tokens: number };
}

/** One decision model a {@link LoomcycleClient.decide} call may name. */
export interface DecisionModel {
  /** What `DecideRequest.model` is set to. */
  name: string;
  provider: string;
  /** The model the provider serves under that name. */
  model: string;
  limits: {
    max_questions: number;
    /** `min_options` and `max_options` bound a choice's options and a score's
     *  levels. */
    min_options: number;
    max_options: number;
  };
}

/** Result of {@link LoomcycleClient.listDecisionModels}. */
export interface DecisionModelsResponse {
  /** The name used when a call names no model. */
  default: string;
  models: DecisionModel[];
}

// ---- Def name enumeration (GET /v1/_<family>def/names) ----
//
// One roll-up per declared name in a substrate family. Tenant-scoped
// server-side: a substrate:tenant principal sees only its own tenant's names;
// an admin sees every tenant's, or focuses one with `tenant`.

/** One name's version roll-up from a `list*DefNames` method. Shared by every
 *  def family except OperatorTokenDef ({@link OperatorTokenDefNameSummary}). */
export interface DefNameSummary {
  name: string;
  /** The owning tenant; absent for the shared "" tenant. */
  tenant_id?: string;
  version_count: number;
  active_def_id?: string;
  latest_version: number;
  /** When the newest version was created (RFC 3339). */
  last_updated: string;
  /** Versions not retired. Reported by the AgentDef, SkillDef, HookDef and
   *  MCPServerDef families only. */
  live_version_count?: number;
  /** The active version is retired. Same families as `live_version_count`. */
  active_retired?: boolean;
}

/** Response of the `list*DefNames` methods. `names` is `[]`, never `null`,
 *  when nothing is declared. */
export interface DefNamesResponse {
  names: DefNameSummary[];
}

/** One (tenant, token name) summary from {@link LoomcycleClient.listOperatorTokenDefNames}.
 *  A per-user (`u-`) name repeats across tenants, so it can appear once per
 *  tenant. Carries no secret material. */
export interface OperatorTokenDefNameSummary {
  name: string;
  tenant_id: string;
  subject: string;
  /** Every token minted under the name in this tenant, rotated and retired ones included. */
  token_count: number;
  /** A non-retired, unexpired token exists. */
  has_current: boolean;
  /** The newest live token's def_id; absent when none is live. Rotate or
   *  retire by it: a per-user name is refused as a target. */
  current_def_id?: string;
  last_updated: string;
}

/** Response of {@link LoomcycleClient.listOperatorTokenDefNames}. */
export interface OperatorTokenDefNamesResponse {
  names: OperatorTokenDefNameSummary[];
}

// ---- Schedules admin (GET/POST /v1/_schedules/...) ----

/** One schedule in {@link LoomcycleClient.listAllSchedules}: a static yaml
 *  `scheduled_runs` entry, a ScheduleDef, or both under one name. */
export interface ScheduleListEntry {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count?: number;
  /** The def id the per-schedule methods take. Absent for a static-only
   *  schedule, which has no ScheduleDef to address. */
  active_def_id?: string;
  latest_version?: number;
  /** When the newest ScheduleDef version was created. Absent for a
   *  static-only schedule. */
  last_updated?: string;
  /** The yaml definition, for a static entry. */
  static_definition?: unknown;
}

/** Response of {@link LoomcycleClient.listAllSchedules}. */
export interface ScheduleListResponse {
  entries: ScheduleListEntry[];
}

/** A run a schedule started and has not finished. */
export interface ScheduleActiveRun {
  def_id: string;
  run_id: string;
  /** The slot the run was fired for. */
  slot_at: string;
  /** Fired for a missed slot rather than a live one. */
  catch_up?: boolean;
  started_at: string;
  /** The replica that claimed the slot; absent outside cluster mode. */
  claimed_by?: string;
}

/** A schedule's runtime state from {@link LoomcycleClient.getScheduleState}.
 *  Credentials never appear here. An optional instant is absent when unset —
 *  test for presence. (A server before the fix that dropped them sent
 *  `last_run_at` and `paused_until` as "0001-01-01T00:00:00Z" instead.) */
export interface ScheduleStateView {
  def_id: string;
  /** Absent until the schedule first fires. */
  last_run_at?: string;
  last_run_id?: string;
  /** completed | failed | cancelled | rejected | running | skipped_overlap | skipped… */
  last_status?: string;
  last_error?: string;
  next_run_at: string;
  /** Present while paused. {@link LoomcycleClient.pauseSchedule} sets it about
   *  100 years ahead: an indefinite pause. */
  paused_until?: string;
  /** Fires so far, the count `max_fires` is measured against. */
  fire_count: number;
  /** The last claim: which slot, which replica, when. */
  slot_at?: string;
  claimed_by?: string;
  claimed_at?: string;
  /** When the last tracked run finished. */
  finished_at?: string;
  /** Present while a `catch_up_max` backlog drains: its newest slot. */
  catch_up_until?: string;
  /** Slots the last outage dropped (older than `catch_up_max`). */
  missed_slots?: number;
  /** Every run the schedule started and has not finished — `[]`, never absent. */
  active_runs: ScheduleActiveRun[];
}

/** Result of {@link LoomcycleClient.runScheduleNow}. The run starts on the
 *  scheduler's next tick, not during the call. */
export interface ScheduleRunNowResult {
  def_id: string;
  scheduled: "next-tick";
}

/** Result of {@link LoomcycleClient.pauseSchedule} / {@link LoomcycleClient.resumeSchedule}. */
export interface SchedulePauseResult {
  def_id: string;
  paused: boolean;
}

// ---- Models, routing, provider models ----

/** One configured model alias (the top-level `models:` map). */
export interface ModelAlias {
  provider: string;
  model: string;
  /** chat | decision | embedder. Absent for an untagged alias, which is a chat
   *  model. */
  kind?: string;
}

/** Response of {@link LoomcycleClient.listModels}: alias name → target. */
export interface ModelAliasesResponse {
  aliases: Record<string, ModelAlias>;
}

/** A candidate's throughput estimate. `multiplier` is what a scaled timeout
 *  would be multiplied by. */
export interface RoutingThroughput {
  slowdown?: number;
  multiplier: number;
  samples: number;
  source: string;
  decode_tps?: number;
  prefill_tps?: number;
  ttft_ms?: number;
  cold_load_ms?: number;
  /** Admin only. */
  queue_ms_p50?: number;
  max_multiplier?: number;
}

/** One candidate in a routing cascade. */
export interface RoutingCandidate {
  provider: string;
  model: string;
  /** First in the displayed cascade. */
  primary: boolean;
  available?: boolean;
  /** The first available candidate: what a run resolves to now. */
  selected?: boolean;
  stalled?: boolean;
  rate_limited?: boolean;
  reachable?: boolean;
  /** Absent when throughput estimation is off. */
  throughput?: RoutingThroughput;
}

/** One tier's ordered cascade, top first. */
export interface RoutingTier {
  /** low | middle | high */
  tier: string;
  cascade: RoutingCandidate[];
}

/** One user tier's tiers. */
export interface RoutingUserTier {
  /** "" when no user tiers are configured. */
  name: string;
  tiers: RoutingTier[];
}

/** One provider in the active-providers header. */
export interface RoutingProvider {
  provider: string;
  reachable: boolean;
  excluded: boolean;
  /** The raw probe error — admin only. */
  last_error?: string;
}

/** One web-search provider in the search cascade. */
export interface SearchRoutingProvider {
  provider: string;
  primary: boolean;
  /** This caller has a usable key. */
  keyable?: boolean;
  /** Keyable and not in a failure cooldown. */
  available?: boolean;
  selected?: boolean;
  reachable?: boolean;
  /** Admin only. */
  last_error?: string;
}

/** Response of {@link LoomcycleClient.getRouting}. */
export interface RoutingResponse {
  generated_at: string;
  /** The caller gets the admin view: raw `last_error`, nothing filtered. */
  admin: boolean;
  providers?: RoutingProvider[];
  user_tiers: RoutingUserTier[];
  /** The operator-key gate is on and this tenant's view is filtered to the
   *  providers it can key itself. */
  operator_key_restricted?: boolean;
  /** `timeout_scaling.mode` when throughput estimation is on. */
  throughput_mode?: string;
  /** Absent when no search providers are configured. */
  search?: SearchRoutingProvider[];
  /** The memory embedder. Absent when none is configured. */
  embedder?: RoutingEmbedder;
  /** The decision models. Absent when none are configured. */
  decision?: RoutingDecision;
}

/** The memory embedder in {@link RoutingResponse}. `state` is what its calls
 *  did since boot, not a probe: `untried` means nothing has been embedded yet.
 *  No error text or address is reported. */
export interface RoutingEmbedder {
  state: "untried" | "ok" | "failing";
  provider?: string;
  model?: string;
  /** The `models:` alias `memory.embedder` names, when it names one. */
  alias?: string;
  /** Absent until the embedder has learned it. */
  dimension?: number;
  calls: number;
  failures: number;
  last_failure_kind?: string;
  last_ok_unix?: number;
  last_failure_unix?: number;
}

/** The decision block in {@link RoutingResponse}. */
export interface RoutingDecision {
  /** The model a call that names none is asked. */
  default: string;
  models: RoutingDecisionModel[];
}

/** One decision model. The state fields are its provider's probe; all three are
 *  absent when the provider was never probed, and `listed` is also absent while
 *  the provider is unreachable. Under the operator-key gate a tenant sees only
 *  the models it can key. */
export interface RoutingDecisionModel {
  /** What a caller names: a `models:` alias or a model name. */
  name: string;
  provider: string;
  model: string;
  default?: boolean;
  /** Reachable and listed. */
  available?: boolean;
  reachable?: boolean;
  /** The provider's model list carries the model (for Ollama: it is pulled). */
  listed?: boolean;
}

/** Response of {@link LoomcycleClient.listProviderModels}: a live listing from
 *  the provider, not the cached probe matrix. */
export interface ProviderModelsResponse {
  provider: string;
  fetched_at: string;
  duration_ms: number;
  /** Provider-native model ids. `[]` when the provider answered with none. */
  models: string[];
}

/** Options of {@link LoomcycleClient.createTeam}. */
export interface CreateTeamOptions {
  signal?: AbortSignal;
  /** Make the new version the team's active one. The server default is true
   *  for a create; pass `false` to author it without putting it in force
   *  (then {@link LoomcycleClient.promoteTeam}). */
  promote?: boolean;
  /** Free-text rationale stored on the version. */
  description?: string;
}

/** Options of {@link LoomcycleClient.forkTeam}. Note `promote` defaults the
 *  other way from a create: false. */
export interface ForkTeamOptions extends CreateTeamOptions {
  /** Make the new version the team's active one. The server default is false:
   *  a fork is authored first and put in force with
   *  {@link LoomcycleClient.promoteTeam}. */
  promote?: boolean;
  /** Fork this version instead of the name's active one. */
  parentDefId?: string;
}

// ---- Memory / document change feeds (RFC CD Part C) ----

/** The kind of change a {@link MemoryChange} names. `memory.*` arrive on
 *  {@link LoomcycleClient.streamMemoryChanges}, `document.chunk.*` on
 *  {@link LoomcycleClient.streamDocumentChanges}. Mirrors the runtime's
 *  `store.MemoryChangeType`. */
export type MemoryChangeType =
  | "memory.set"
  | "memory.delete"
  | "memory.scope_deleted"
  | "document.chunk.updated"
  | "document.chunk.deleted";

/** The memory scope a change landed in. `global` is listed because the
 *  runtime's scope enum has it and the feed passes the written scope through
 *  unchanged, not because Memory writes it today. */
export type MemoryChangeScope = "agent" | "user" | "tenant" | "global";

/** One change coordinate, exactly as the runtime's `store.MemoryChange`
 *  serialises.
 *
 *  VALUE-FREE by design: it names WHAT changed, never the value — pull the
 *  current value through the data API (`getMemoryEntry`, `document`). `key` is
 *  set for `memory.set` / `memory.delete`; `chunk_id` for `document.chunk.*`;
 *  neither for `memory.scope_deleted`, which covers the whole scope. */
export interface MemoryChange {
  /** Monotonic cursor. Persist the last one you processed and pass it back as
   *  `since` to resume after a disconnect. */
  seq: number;
  /** The caller's own tenant — the feed is never cross-tenant. Omitted for
   *  the shared "" tenant. */
  tenant?: string;
  type: MemoryChangeType;
  scope: MemoryChangeScope;
  scope_id: string;
  key?: string;
  chunk_id?: string;
  /** RFC3339 time of the write. */
  at: string;
}

/** What the runtime's embedder has actually been doing, since boot. Mirrors
 *  `providers.EmbedderHealth`.
 *
 *  Carried on the change feed because an embedding failure on a content write
 *  is NOT fatal — the body is stored and the embedding skipped — so an
 *  embedder outage shows as a perfectly busy feed over rows that are no longer
 *  searchable. `untried` is "configured, nothing embedded yet", not a warning;
 *  `absent` is "no embedder configured". */
export interface EmbedderHealth {
  state: "absent" | "untried" | "ok" | "failing";
  provider?: string;
  model?: string;
  calls: number;
  /** Failed embeddings since boot — roughly how many rows need
   *  `backfillEmbeddings` to become searchable. */
  failures: number;
  /** A classified failure kind, never the raw error text (which can carry an
   *  address). */
  last_failure_kind?: string;
  /** Unix seconds; absent when never. */
  last_ok_unix?: number;
  last_failure_unix?: number;
}

/** The opening frame of a change feed (`event: feed`). */
export interface ChangeFeedStatus {
  /** Whether writes are being captured at all. `false` means the server runs
   *  without `LOOMCYCLE_MEMORY_CHANGES_ENABLED`: the stream stays open and
   *  idles, and NO change will ever arrive — check this before waiting. */
  enabled: boolean;
  /** The cursor the server accepted. `0` when `since` was absent OR
   *  unparseable — the server does not reject a bad cursor. */
  since: number;
  /** Reported beside `enabled`, never folded into it: capturing and embedding
   *  fail independently. Absent on a runtime older than the field. */
  embedder?: EmbedderHealth;
}

/** A change-feed `event: error` frame: the server could not read its change
 *  log. NON-FATAL — the stream stays open, the server keeps retrying with
 *  backoff, and its cursor does not advance, so change frames resume after
 *  recovery without a gap. Sent once per failure streak, not per retry. */
export interface ChangeFeedError {
  /** Machine-readable; `store_unavailable` today. Branch on this. */
  code: string;
  /** Human-readable description. */
  error: string;
}

/** Yielded by {@link LoomcycleClient.streamMemoryChanges} and
 *  {@link LoomcycleClient.streamDocumentChanges}.
 *
 *  Same `{ kind, payload }` shape as {@link RunStateStreamItem}: the first
 *  item is `{ kind: "feed" }`, then one `{ kind: "change" }` per change, with
 *  an occasional `{ kind: "error" }` (see {@link ChangeFeedError}) when the
 *  server cannot read its change log — iteration continues past it. `kind`
 *  rather than `type` so the wrapper's discriminator cannot be confused with
 *  the change's own `payload.type`. */
export type ChangeFeedFrame =
  | { kind: "feed"; payload: ChangeFeedStatus }
  | { kind: "change"; payload: MemoryChange }
  | { kind: "error"; payload: ChangeFeedError };

/** Options for {@link LoomcycleClient.streamMemoryChanges} /
 *  {@link LoomcycleClient.streamDocumentChanges}. */
export interface StreamChangesOptions {
  /** Resume cursor: deliver only changes with `seq` greater than this. Omit
   *  (or `0`) to start from the oldest change the server still retains. Must
   *  be a non-negative integer — the client throws rather than send a value
   *  the server would silently read as `0` and replay everything. */
  since?: number;
  /** Abort to close the stream; the iterator then throws an `AbortError`. */
  signal?: AbortSignal;
}

// ---- Access control: groups, rule lists, bindings, decisions (RFC EY / EZ) ----
//
// Every management call acts in the caller's own tenant, server-side; the
// `tenant` option is an admin's focus on another tenant (?tenant=), ignored
// for a tenant operator. Groups, lists and bindings are the tenant operator's
// (`substrate:tenant`, admin, or the legacy shared token); any principal may
// ask the decision routes, about itself. Lists and bindings the config
// declares are listed beside the stored rows with `source: "config"` and are
// read-only; they carry fewer fields (no version, no timestamps), so those
// fields are optional here.

/** The access-control rollout mode a deployment runs in. */
export type AclMode = "compat" | "shadow" | "enforce";

/** Marks an entry the config declares rather than the store holds. */
export type AclSource = "config";

export type AclEffect = "allow" | "deny";

/** What a binding attaches a list to. `tenant` is everyone in the tenant. */
export type AclSubjectKind = "user" | "group" | "token" | "tenant";

/** The closed `when` set of a rule. Every present field narrows the rule; an
 *  absent one does not. */
export interface AclCondition {
  /** The request arrived on one of these (see `AclCatalog.transports`). */
  transport?: string[];
  /** A wall-clock window: on one of `days` (`mon`…`sun`), inside
   *  `between: [from, to)` (`HH:MM`), in `tz` (an IANA zone). */
  time?: { days?: string[]; between?: string[]; tz?: string };
  interactive?: boolean;
  tier?: string[];
  user_tier?: string[];
  source_cidr?: string[];
  owner_is_self?: boolean;
  /** Inside a run: the run's agent matches this glob. */
  agent?: string;
  /** The run's spawn depth is at most `max`. */
  depth?: { max: number };
}

/** One rule: `effect` on any of `actions` over any of `resources`
 *  (`type:selector`, e.g. `agent:reviewer-*`), when `when` holds. */
export interface AclRule {
  effect: AclEffect;
  actions: string[];
  resources: string[];
  when?: AclCondition;
  description?: string;
}

/** A rule list as written: a built-in baseline, or a draft list for
 *  {@link LoomcycleClient.authzSimulate}. */
export interface AclList {
  name: string;
  description?: string;
  rules: AclRule[];
}

/** One rule list: a stored one, or (`source: "config"`) one the config
 *  declares, which has only a name, a description and its rules. */
export interface AclDefRow {
  tenant_id?: string;
  name: string;
  /** Bumped on every write. Absent on a config list. */
  version?: number;
  content_sha256?: string;
  catalog_version?: number;
  rules: AclRule[];
  description?: string;
  created_at?: string;
  updated_at?: string;
  created_by?: string;
  /** Set once the list is retired; absent while it is live. */
  retired_at?: string;
  source?: AclSource;
}

/** `aclDef({op: "list"})`. `builtin` is the code-defined baselines, which no
 *  call can change. */
export interface AclDefListResponse {
  acls: AclDefRow[];
  builtin: AclList[];
}

/** `GET /v1/_acldef/names`: every live list name the tenant's bindings can
 *  reach, stored and config, sorted. */
export interface AclDefNamesResponse {
  names: string[];
}

/** `aclDef({op: "validate"})` on rules the server accepts. Rules it refuses
 *  are a 400 (`acl_invalid`) naming the fault. */
export interface AclValidateResult {
  valid: boolean;
  /** How many rules were read. */
  rules: number;
  catalog_version: number;
}

/** One resource type of the catalog: its verbs, and for a scoped type the
 *  scopes a selector may name (`separator` joins scope and name). */
export interface AclCatalogType {
  type: string;
  verbs: string[];
  scopes?: string[];
  separator?: string;
}

/** `aclDef({op: "catalog"})`: what a rule may name. */
export interface AclCatalog {
  catalog_version: number;
  types: AclCatalogType[];
  transports: string[];
}

/** The ops of the AclDef family. `create`, `update` and `put` are the same
 *  upsert. */
export type AclDefOp =
  | "create"
  | "update"
  | "put"
  | "get"
  | "list"
  | "retire"
  | "validate"
  | "catalog"
  | "names";

/** The input of {@link LoomcycleClient.aclDef}. `name` is required by
 *  create/update/put/get/retire, `rules` by create/update/put/validate,
 *  `include_retired` is read by list. */
export interface AclDefInput {
  op: AclDefOp;
  name?: string;
  rules?: AclRule[];
  description?: string;
  include_retired?: boolean;
}

/** One group: a stored one, or (`source: "config"`) one the config declares,
 *  which lists its direct `members` (subjects) and member `groups` inline and
 *  has no timestamps. A stored group's members come from
 *  {@link LoomcycleClient.getGroup}. */
export interface GroupRow {
  tenant_id?: string;
  name: string;
  description?: string;
  /** Set on a group an identity provider owns: its members are managed there,
   *  and a write here is refused (409 `group_external`). */
  external_id?: string;
  created_at?: string;
  updated_at?: string;
  created_by?: string;
  source?: AclSource;
  members?: string[];
  groups?: string[];
}

export type GroupMemberKind = "user" | "group";

/** One direct membership edge of a stored group. */
export interface GroupMember {
  tenant_id?: string;
  group: string;
  member_kind: GroupMemberKind;
  member_id: string;
  added_at: string;
  added_by?: string;
}

export interface GroupListResponse {
  groups: GroupRow[];
}

/** `GET /v1/_groups/{name}`: a stored group with its direct members. */
export interface GroupDetail {
  group: GroupRow;
  members: GroupMember[] | null;
  /** Every group a member of this one is also in, transitively, this one
   *  included. */
  contained_in: string[] | null;
}

/** `GET /v1/_groups/{name}/effective`: everyone the group holds, at any
 *  nesting depth, config-declared members included. */
export interface GroupEffective {
  group: string;
  /** User subjects, sorted. */
  users: string[];
  /** The nested groups walked, sorted (the group itself excluded). */
  groups: string[];
}

/** The body of {@link LoomcycleClient.createGroup}: an upsert by name. */
export interface CreateGroupBody {
  name: string;
  description?: string;
  external_id?: string;
}

/** What a membership write reports. `removed` is set by a removal. */
export interface GroupMemberChange {
  group: string;
  kind: GroupMemberKind;
  id: string;
  removed?: boolean;
}

/** One binding of a list to a subject: a stored one, or (`source: "config"`)
 *  one the config declares, which has no `binding_id` and no timestamps. */
export interface AclBinding {
  binding_id?: string;
  tenant_id?: string;
  acl: string;
  subject_kind: AclSubjectKind;
  /** Absent for a tenant-wide binding. */
  subject_id?: string;
  /** Absent when the binding never expires. */
  expires_at?: string;
  created_at?: string;
  created_by?: string;
  /** Set once retired; absent while live. */
  retired_at?: string;
  source?: AclSource;
}

export interface AclBindingListResponse {
  bindings: AclBinding[];
}

/** The body of {@link LoomcycleClient.createAclBinding}. The list must exist
 *  and `expires_at` (RFC 3339) must be in the future, or the server refuses
 *  with 400 `acl_binding_invalid`. */
export interface CreateAclBindingBody {
  acl: string;
  subject_kind: AclSubjectKind;
  subject_id?: string;
  expires_at?: string;
}

/** One rule that decided: list, its version, the rule's index, its effect. */
export interface AuthzReason {
  acl: string;
  version?: number;
  rule: number;
  effect: AclEffect;
}

/** An access-control decision (AuthZEN 1.0 shape). `decision` is true only
 *  when a rule allowed; `outcome` `no_match` means no rule spoke, which in
 *  `compat` and `shadow` mode the scope gate then decides. */
export interface AuthzDecision {
  decision: boolean;
  outcome: "allow" | "deny" | "no_match";
  reasons?: AuthzReason[];
  /** The request crossed a tenant and no rule was read. */
  tenant_floor?: boolean;
  mode: AclMode;
}

/** The request context a rule's `when` is judged against. Omitted fields take
 *  the call's own (`transport` is the transport the call arrived on). */
export interface AuthzRequestContext {
  transport?: string;
  interactive?: boolean;
  tier?: string;
  user_tier?: string;
  agent?: string;
  depth?: number;
}

/** One question: may `subject` do `action` to `resource`? An omitted
 *  `subject` (or one naming the caller) asks about the caller; naming another
 *  subject of the tenant needs a tenant operator. An empty `resource.id` asks
 *  about any instance of the type. */
export interface AuthzEvaluationRequest {
  subject?: { type?: string; id?: string };
  action: { name: string };
  resource: { type: string; id: string; owner?: string };
  context?: AuthzRequestContext;
}

/** How {@link LoomcycleClient.authzEvaluations} runs its batch. */
export type AuthzEvaluationsSemantic =
  | "execute_all"
  | "deny_on_first_deny"
  | "permit_on_first_permit";

/** A batch of questions for one subject. A top-level `action`, `resource` or
 *  `context` is the default each entry that omits its own inherits. 1 to 256
 *  entries. */
export interface AuthzEvaluationsRequest {
  subject?: { type?: string; id?: string };
  action?: { name: string };
  resource?: { type: string; id: string; owner?: string };
  context?: AuthzRequestContext;
  evaluations: Array<Partial<Omit<AuthzEvaluationRequest, "subject">>>;
  options?: { evaluations_semantic?: AuthzEvaluationsSemantic };
}

export interface AuthzEvaluationsResponse {
  evaluations: AuthzDecision[];
}

/** One rule the engine read for an explanation, matching or not, and why. */
export interface AuthzTrace {
  acl: string;
  version?: number;
  rule: number;
  effect: AclEffect;
  matched: boolean;
  why: string;
}

/** A binding as the engine applied it. */
export interface AuthzAppliedBinding {
  id?: string;
  acl: string;
  subject_kind: AclSubjectKind;
  subject_id?: string;
  expires_at?: string;
}

/** `POST /v1/_authz/explain`: the decision, every rule read for the resource's
 *  type, the bound lists that do not exist, the bindings applied, and the
 *  subject's groups. */
export interface AuthzExplainResponse {
  decision: AuthzDecision;
  traces: AuthzTrace[] | null;
  missing_acls: string[] | null;
  applied_bindings: AuthzAppliedBinding[] | null;
  groups: string[] | null;
}

/** A draft binding for {@link LoomcycleClient.authzSimulate}. */
export interface AuthzDraftBinding {
  acl: string;
  subject_kind: AclSubjectKind;
  subject_id?: string;
  expires_at?: string;
}

/** Evaluate questions against a DRAFT: the subject's lists and bindings now,
 *  with `acls` added or replaced by name and `bindings` added. Nothing is
 *  written. 1 to 256 requests. */
export interface AuthzSimulateRequest {
  subject?: { type?: string; id?: string };
  acls?: AclList[];
  bindings?: AuthzDraftBinding[];
  requests: AuthzEvaluationRequest[];
}

export interface AuthzSimulateResult {
  /** The request, echoed. */
  request: AuthzEvaluationRequest;
  now: AuthzDecision;
  with_draft: AuthzDecision;
  /** The outcome differs between `now` and `with_draft`. */
  changed: boolean;
}

export interface AuthzSimulateResponse {
  results: AuthzSimulateResult[];
  /** Lists the draft's bindings name that no layer has. */
  missing_acls: string[] | null;
}

// ---- App sign-in (RFC FL) ----
//
// loomcycle runs a small OAuth 2.0 authorization server for its own apps: an
// app sends the person to loomcycle's login page, they sign in through
// whatever gates the operator configured, and the app gets a short-lived
// access token for that same user plus a rotating refresh token. See
// LoomcycleOAuth (oauth.ts) for the flow and OAuthTokenProvider for a client
// that refreshes on its own.

/** The authorization server's metadata (RFC 8414), from
 *  `GET /.well-known/oauth-authorization-server`. Published only when the
 *  server knows its public address (`LOOMCYCLE_PUBLIC_URL`). */
export interface OAuthServerMetadata {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  revocation_endpoint: string;
  response_types_supported: string[];
  grant_types_supported: string[];
  code_challenge_methods_supported: string[];
  token_endpoint_auth_methods_supported: string[];
  revocation_endpoint_auth_methods_supported: string[];
  /** The server adds `iss` to every authorization response (RFC 9207). */
  authorization_response_iss_parameter_supported: boolean;
}

/** The token endpoint's answer (RFC 6749 §5.1). */
export interface OAuthTokenResponse {
  access_token: string;
  /** Always `Bearer`. */
  token_type: string;
  /** Seconds the access token lives. */
  expires_in: number;
  /** Single use: each refresh returns a new one and spends the old one.
   *  Presenting a spent one ends the whole sign-in. */
  refresh_token: string;
}

/** A token answer with the moment the access token expires — what a caller
 *  persists. */
export interface OAuthTokenSet extends OAuthTokenResponse {
  /** When the access token expires, in milliseconds since the epoch
   *  (`Date.now()` at receipt plus `expires_in`). */
  expires_at: number;
}

/** Input to `LoomcycleOAuth.beginAuthorize`. */
export interface BeginAuthorizeOptions {
  /** The app's registered client id. */
  clientId: string;
  /** One of the app's registered redirect URIs, exactly (a loopback one may
   *  carry any port). */
  redirectUri: string;
  /** An opaque value to round-trip; a random one is generated when omitted.
   *  1 to 1024 characters. */
  state?: string;
}

/** What `LoomcycleOAuth.beginAuthorize` returns. Persist `verifier` and
 *  `state` (session storage, never a URL) until the redirect comes back, then
 *  send the person to `url`. */
export interface AuthorizeRequest {
  url: string;
  /** The PKCE code verifier. A secret until the exchange: keep it out of
   *  URLs and logs. */
  verifier: string;
  state: string;
}

/** The parameters the server puts on the redirect back to the app: `code`
 *  on success, `error` (+ `error_description`) otherwise, `state` as sent,
 *  and `iss` when the server knows its issuer (RFC 9207). */
export interface AuthorizeResponseParams {
  code?: string;
  state?: string;
  iss?: string;
  error?: string;
  error_description?: string;
}

/** Input to `LoomcycleOAuth.completeAuthorize`: the redirect's parameters
 *  plus what `beginAuthorize` returned. */
export interface CompleteAuthorizeOptions extends AuthorizeResponseParams {
  /** The `state` beginAuthorize returned; a redirect carrying any other is
   *  refused before the code is used. */
  expectedState: string;
  verifier: string;
  clientId: string;
  /** The same redirect URI the authorization request named. */
  redirectUri: string;
  signal?: AbortSignal;
}

/** Input to `LoomcycleOAuth.refresh`. */
export interface RefreshTokenOptions {
  refreshToken: string;
  clientId: string;
  signal?: AbortSignal;
}

/** Input to `LoomcycleOAuth.revoke`. */
export interface RevokeTokenOptions {
  /** A refresh token (ends the whole sign-in) or an access token (ends that
   *  token only). */
  token: string;
  clientId: string;
  signal?: AbortSignal;
}

/** Supplies the bearer for every request of a `LoomcycleClient` built with
 *  `tokenProvider` instead of `authToken`. `OAuthTokenProvider` implements it
 *  for an app sign-in; any other source of short-lived tokens can too. */
export interface TokenProvider {
  /** The token for the next request, refreshed ahead of expiry; undefined
   *  sends none. A throw fails the request with that error. */
  getToken(): Promise<string | undefined>;
  /** Called at most once per request, after the server answered 401 to
   *  `rejected`: a different token to retry with once, or undefined to let
   *  the 401 stand. */
  onUnauthorized?(rejected: string | undefined): Promise<string | undefined>;
}

/** A client's refresh-token lifetimes: Go durations or whole days (`"7d"`). */
export interface AppClientRefresh {
  idle?: string;
  absolute?: string;
}

/** An app client's definition: an entry of the config's `auth.clients`, and
 *  the body of a ClientDef. */
export interface AppClientConfig {
  /** `public`, the only kind: the app holds no secret and PKCE binds a code
   *  to it. */
  kind: string;
  /** The name the login page shows. */
  label?: string;
  /** Exact https URIs, loopback URIs (any port, exact path) and private-use
   *  scheme URIs (`myapp://callback`). */
  redirect_uris: string[];
  /** Origins allowed to call the token and revoke endpoints from a browser:
   *  https, loopback, `chrome-extension://`, `moz-extension://`, `tauri://`. */
  allowed_origins?: string[];
  /** Go duration, 1m to 24h; default 1h. */
  access_token_ttl?: string;
  refresh?: AppClientRefresh;
  /** Narrows every token the client's sign-ins mint, as a token's
   *  restrictions record does. */
  restrictions?: Record<string, unknown>;
}

/** The body of `POST /v1/_clientdef`. */
export interface ClientDefInput {
  op: "create" | "update" | "get" | "list" | "retire" | "verify";
  /** The client id. Unique across the deployment, retired names included. */
  name?: string;
  def_id?: string;
  client?: AppClientConfig;
  description?: string;
  /** Admin only. On create: the tenant whose people the client signs in, ""
   *  (the default) for every tenant. On list: focus one tenant. A tenant
   *  operator's clients are always its own tenant's. */
  tenant?: string;
  /** verify: the content hash to compare with the deployed one. */
  content_sha256?: string;
}

/** A runtime-registered client (a ClientDef version). */
export interface ClientDefRecord {
  name: string;
  source: "def";
  def_id: string;
  version: number;
  /** The tenant whose people it signs in; "" for every tenant. */
  tenant_id: string;
  all_tenants: boolean;
  label: string;
  redirect_uris: string[];
  /** The definition as stored. */
  client: AppClientConfig;
  description: string;
  content_sha256: string;
  created_at: string;
  created_by: string;
  created_by_tenant: string;
  read_only: false;
}

/** A client declared in the server's config: read only through the API. */
export interface ConfigAppClient {
  name: string;
  source: "config";
  label: string;
  all_tenants: true;
  tenant_id: string;
  redirect_uris: string[];
  allowed_origins: string[];
  access_token_ttl: string;
  refresh_idle: string;
  refresh_absolute: string;
  read_only: true;
}

/** A client as `get` and `list` report it. */
export type AppClientEntry = ClientDefRecord | ConfigAppClient;

/** `op: "list"`: the clients the caller may see, config ones included. */
export interface ClientDefListResponse {
  clients: AppClientEntry[];
}

/** `op: "verify"`: whether a client is deployed and matches a hash. An absent
 *  client (or one the caller may not see) answers `deployed: false`. */
export interface ClientDefVerifyResult {
  name: string;
  deployed: boolean;
  current_def_id?: string;
  current_sha256?: string;
  version?: number;
  matches: boolean;
}

/** `op: "retire"`. */
export interface ClientDefRetireResult {
  name: string;
  retired: boolean;
}

/** One row of `GET /v1/_clientdef/names`. */
export interface ClientDefNameSummary {
  name: string;
  source: "def" | "config";
  tenant_id: string;
  label: string;
  read_only: boolean;
}

export interface ClientDefNamesResponse {
  names: ClientDefNameSummary[];
}

// ---- Device login (RFC 8628, a `kind: device` sign-in gate) ----

/** What to show the person: open `verificationUri` and type `userCode`, or
 *  open `verificationUriComplete`, which carries the code. */
export interface DeviceLoginPrompt {
  userCode: string;
  verificationUri: string;
  verificationUriComplete?: string;
  /** Seconds until the code expires. */
  expiresIn: number;
}

export interface DeviceLoginOptions {
  /** How the client names itself on the approval page (default "loomcycle-ts"). */
  clientId?: string;
  /** Called once with the code to show; polling starts after it returns. */
  onPrompt: (prompt: DeviceLoginPrompt) => void | Promise<void>;
  signal?: AbortSignal;
}

/** The token issued to the person who approved the login: their own member
 *  token, never wider than the session they approved from and never living
 *  past it. Keep it in an OS credential store, never a plain file. */
export interface DeviceLoginResult {
  accessToken: string;
  tokenType: string;
  /** Seconds until the token expires. */
  expiresIn?: number;
  scope?: string[];
}
