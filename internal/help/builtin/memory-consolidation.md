---
name: memory-consolidation
description: How background memory consolidation works — why add is async, what a scheduled consolidator does with the queue, what origin/class/provenance mean on a stored fact, and the operator knobs.
---
`Memory op=add` does not store a fact. It **enqueues a conversation** and
returns `pending`. Something else — a scheduled *consolidator* — later reads
settled chats and the queue, decides what is worth keeping, and writes the
durable facts.

This topic is what that "something else" does, and what it leaves behind on
the rows you `recall`.

## Why add is asynchronous

Deciding which sentences in a conversation are durable is a judgement call:
"I prefer tabs" is durable, "leave the ticket in-progress" is not, and "I
switched to spaces" *contradicts something you already stored*. Making that
judgement needs a model call.

Doing it inline would mean every `add` pays for an LLM round-trip and blocks
the turn that made it — so `add` writes to a durable queue and returns
immediately.

The consequence is the one thing to remember: **there is no read-after-write.**

```json
{"op": "add", "scope": "user", "messages": [{"role": "user", "content": "I'm based in Berlin"}]}
```
→ `{"status": "pending", "event_id": "pend_…"}`

A `recall` right after that `add` will **not** find "Berlin". It becomes
recallable once a consolidation pass has run. If you need a fact readable
immediately, use `set` with your own key (or `add` with `infer: false`, which
stores the turns verbatim as one row) — those are synchronous.

## What a consolidation pass does

A pass is not a subsystem — it is an agent following a procedure, and the
procedure is the bundled consolidator's **system prompt**. That is
deliberate: deciding what is durable is a judgement call an operator should
be able to read and change. It is inlined rather than loaded on demand
because the indirection cost real reliability — a small local model given
the procedure behind a tool call made no tool calls at all, and drove the
same pass correctly once the steps were in front of it.

A pass works on **one memory target** (one scope + scope id) and walks a
fixed sequence:

1. **Take the lease** so two passes can't work the same target at once.
2. **Read the watermark** — how far consolidation has already progressed.
3. **Scan for settled chats past it**, oldest first. Only finished chats
   qualify, so a live conversation is never half-read.
4. **Read those transcripts**, and **drain the queue** of pending `add`s.
5. **Recall what is already stored**, so it can update instead of duplicating.
6. **Write** the facts: `set` for new or refined ones, `supersede` for ones
   the conversation contradicts.
7. **Advance the watermark** to the newest chat it consolidated, and
   **release the lease**.

Two properties fall out of that shape and are worth relying on:

- **A pass is safe to re-run.** Facts are keyed by their *subject*, not by
  when they were extracted, so re-consolidating the same chats overwrites the
  same rows instead of growing near-duplicates beside them.
- **A pass is resumable.** The watermark is forward-only, so an interrupted
  pass costs at most a repeat of the chats it had not finished.

## Superseding, not deleting

When a conversation contradicts a stored fact, the pass **archives** the old
row rather than deleting it: `supersede` hides it from `get` and `list` — and
from `search` / `recall` wherever the vector or full-text stack is configured
(on SQLite without vectors there is no search leg to hide it from) — but the
row survives. Writing the same key again revives it — which matters when a
preference flips back.

So a fact disappearing from `recall` does not mean it was destroyed. It means
something more recent replaced it.

## Provenance: where a fact came from

Every consolidated row carries an audit trail, and you can read it back:

| Field | What it means |
|---|---|
| `origin` | `consolidator` when a consolidation pass distilled this fact from a transcript. Stamped by the server from the pass's own grant — it cannot be claimed by an agent that just writes a key. |
| `class` | What KIND of fact it is: `preference`, `constraint`, `correction`, `fact`, … Assigned by the pass. |
| `source_session_id` | The chat it was distilled from. |
| `source_run_id` | The run within that chat. |

`origin` is the trustworthy filter for "a machine derived this from a
conversation" as opposed to "an agent wrote it deliberately". `class` is how
facts get grouped, aged, and explained. The source ids are what make a fact
**traceable back to the words that produced it** — and what makes it possible
to remove everything derived from a person when they ask.

Because of that last point: **never consolidate a credential, token, key, or
password into memory.** A transcript is transient; a consolidated fact is
long-lived, broadly readable within its scope, and surfaced *unprompted* by
recall. Relaying a secret into it converts a passing exposure into a durable
one. The same goes for transient state — task status, pleasantries, "I'll do
that after lunch" — which is stale by the next pass and only crowds out what
matters.

## Running a pass yourself

The consolidation ops (`cursor_get`, `cursor_scan`, `cursor_lease`,
`cursor_advance`, `cursor_release`, `supersede`, `pending_drain`,
`pending_ack`) are gated by a **separate grant** from `memory_scopes`. An
agent with full memory access still cannot touch them without it — otherwise
any memory-capable agent could move another pass's watermark or archive facts
it did not write.

If you have the grant, the sequence above is the one to follow, and two rules
are enforced by the server rather than left to you:

- `cursor_advance` is checked against a **real settled chat in your own
  tenant belonging to this target**. You cannot jump the watermark forward to
  a timestamp you invented, so a transcript line that *looks* like cursor
  bookkeeping cannot stop this target's consolidation.
- `cursor_scan` only ever returns chats **strictly after** the stored
  watermark, oldest first, and never your own past passes — so consolidating
  a page and advancing to its last row cannot skip anything.
- The three ops that CHANGE bookkeeping — `cursor_advance`, `supersede`,
  `pending_ack` — additionally require that **you hold the target's lease**.
  Two passes over one target are possible (the dispatcher's lock is per
  schedule, and an operator can start a pass by hand), and an ack is the one
  step with no recovery: it marks queued turns drained, so a pass that acks
  and then fails has removed them from every future pass's view. A plain `set`
  is deliberately not lease-checked — deterministic keys make a concurrent
  write overwrite the same row rather than duplicate it.

## Operator knobs

| Setting | Effect |
|---|---|
| `LOOMCYCLE_MAX_CONSOLIDATION_TARGETS` | Most targets one tick may dispatch (default 32). Targets beyond it wait for the next tick; the watermark makes that safe. |
| `LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY` | Parallel passes per tick (default 4). Forced to 1 when the passes resolve to a local model runtime. |
| `memory_quota_bytes` / `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` | Per-scope byte cap. A consolidation write over budget is **refused, loudly** — it does not silently drop the fact. |
| `memory.consolidation.merge_threshold` | Similarity at or above which two facts count as the same fact reworded and get merged (default `0.95`). |
| `memory.consolidation.related_threshold` | Lower edge of "overlapping subject, different claim", which is added rather than merged (default `0.85`). |

### Calibrating the two bands

Cosine scale is a property of the **embedding model**, not a universal, so a
default band is calibrated to at most one model — and being wrong is silent in
both directions. A band nothing reaches never merges, so duplicates accumulate
forever with no error anywhere. A band everything reaches merges two distinct
facts into one, which is data loss.

Measured on a 768-dim `embeddinggemma` with a 12-fact corpus and 24 labelled
probes, the **highest** genuine paraphrase scored `0.9487` — just under the
`0.95` default. On that model the shipped band merges **0 of 12** duplicates.
The safe window there is `(0.6775, 0.7181]`: merging at 0.68–0.70 catches all
12 duplicates and makes zero false merges.

The defaults stay `0.95` / `0.85` anyway, because the risk is asymmetric: too
high leaves duplicates lying around and every one of them is still recoverable,
while too low destroys a distinct fact and that is not. `0.95` fails safe.

An operator measures their own model with `loomcycle memory-calibrate`, which
reports each class's distribution, a threshold sweep, and a recommendation, and
exits non-zero when no threshold can separate duplicates from related facts.
Two caveats worth knowing: on this model the RELATED and UNRELATED classes
**overlap** (no threshold separates them, so the related band is a
recall/false-positive trade-off rather than a clean split), and a calibration
belongs to one model — **changing the embedding model or its dimension
invalidates it**.

An agent can read the effective bands at runtime from
`Context op=capabilities` → `consolidation`; the consolidator agent also
receives them in its system prompt via `{{memory:consolidation_bands}}`.

Consolidation is **opt-in**: without a schedule pointing at a consolidator
agent, `add` still queues durably and nothing drains it. Queued items are not
lost, they are simply not yet distilled.

See the `memory-layer` topic for the `add` / `recall` op reference,
`memory-ranking` for how `recall` orders results, and `scheduled-runs` for
wiring the schedule that drives a pass.
