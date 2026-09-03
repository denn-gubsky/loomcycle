# LoomCycle release history

Per-version release notes from v0.4.0 onward. The current and immediately previous releases are also summarised in the main [`README.md`](README.md); older releases live here.

For the **public roadmap** (planned v0.8.16 through v1.0 work — Question tool, Pause / Resume / Snapshot, distribution, operator postures), see [`docs/PLAN.md`](docs/PLAN.md).

## What's in v1.73.0

**Recall over what distillation threw away (#1129, RFC CT P1).** Every context
retention mode in this runtime discards work by design: a compaction drops the
turns behind its cut, a recap keeps the reasoning and drops the rest, and a
stateful step feeds forward `(Σ, O)` and discards how it got there. That is the
point of them — but the dropped span is often exactly where the one value the
model now needs was stated.

An opt-in per-agent `context.recall` harvests each evicted span, at the last
moment it exists, into a run-scoped embedded index, and gives the agent one
`Recall(query)` tool to read it back. **Free text rather than identifiers**,
because a model queries fluently in plain language and barely reproduces ids or
its own exact prior wording — which also makes the tool far likelier to be
invoked at all. It searches the run index, silently falls back to the agent's
durable memory across its permitted scopes, merges by score, and returns the
originals verbatim.

Run-scoped and in-memory deliberately: the persistent vector store is per-scope
and durable, so indexing every evicted turn there would pollute the agent's
memory and add store writes to the distillation hot path. A harvest is never
fatal, a nil embedder makes both halves a clean no-op, and the index is
FIFO-capped. The resume path is deliberately **not** wired — a resumed run
replays its past distillations without harvesting, so nothing double-indexes.

Off by default: unopted runs and no-embedder deployments are byte-identical, and
`recall` threads through the same content-identifying plumbing as the rest of the
context block, so a fork that flips it mints a distinct `content_sha256` while
every pre-feature agent row stays byte-stable.

**The Memory console honours the tenant focus (#1130).** The last hop of the
admin-focus fix below. The server had accepted `?tenant=` since v1.72.0 and
`@loomcycle/client` 1.72.0 could send it, but the console was not asking, so a
super-admin's Memory page could only read its own tenant — usually the empty one
— while that tenant's operator saw its rows. Every other browse surface
(Documents, Paths, Agents, Users) already consumed the topbar focus; Memory was
the one that never did.

The focus binds at data-layer **construction**, not per call. Which tenant's
workspace an operator is looking at is a property of the console session, so
threading it through `MemoryDataLayer` would have put the same never-varying
value into twenty call sites and changed a 21-method interface to carry it. A
`browse` option captured by `dataLayerFromClient` reaches every method without
any of them knowing it exists. `listScopes` deliberately does not carry it: that
route answers "what *kinds* of scope exist", a constant set, and sending a tenant
would imply the answer varies by tenant.

Verified on a live deployment before the tag, with two tenants holding 0 and 136
rows: an admin naming none still gets 0 (the default is unmoved), an admin naming
the tenant gets 136 (the capability is real), and a tenant operator naming
*another* tenant gets its own 136 — the wire value ignored, not honoured and then
checked.

`@loomcycle/memory-view` is 0.5.0, with its `@loomcycle/client` peer raised to
`^1.72.0` so a consumer resolving an older client gets a dependency error rather
than a console that silently sends nothing. The TS adapter is unchanged at
1.72.0 and the Python adapter at 1.67.0.

## What's in v1.72.0

Three fixes found by *using* the runtime rather than reading it. Each was an
operator question the console answered wrongly, or not at all.

**A super-admin may focus a tenant on the memory browse routes (#1127).** An
admin was strictly **less** capable than a tenant operator here, which is
backwards: `substrate:admin` satisfies every scope gate and then could not read
the data the gate admits. The five memory browse handlers resolved the tenant
from the bearer and took nothing from the URL, so a `substrate:tenant` token read
its own tenant's memory while an admin saw only its own — usually empty — and had
no parameter available to look elsewhere. From the console that reads as a
permissions failure.

The inconsistency was inside one file: the sibling maintenance routes on the same
store — embed stats, reembed, backfill, purge — had accepted `?tenant=` via
`principalTenantScope` all along. Only the reads and writes an operator actually
browses with were pinned.

Only an **explicit** focus widens. With no `?tenant=` the resolution is
byte-identical to before, so this adds a capability rather than moving a default,
and a non-admin's wire value is ignored rather than honoured-then-checked — no
tenant can widen its own scope.

Three helpers resolve a tenant and they disagree on the admin default. That
disagreement is irreducible: a **list** read can mean "every tenant" while a
single-tuple read must name exactly one. It is now written down once, naming all
three and the invariant they share, with every cell of the table pinned by a
test. What made it a defect was not the difference — it was that nothing
asserted it, so the difference read as an accident, and on these routes it was.

**The Library shows what an agent is allowed to reach (#1126).** An operator
asking "what can this agent do?" got half an answer, and it cost real diagnosis
time: while checking why memory placement was not working, the Library showed no
`sql_scopes` at all, which reads as the grant being absent. The grant was
present. Only the field was missing.

Two independent gaps produced one symptom. The wire shape was a hand-maintained
third mirror of the substrate agent shape — its own comment said mirroring was
its purpose — and it had drifted to **19 of 46 fields**: `sql_scopes`,
`history_scope`, `memory_consolidation`, `internal`, `sampling`, `compaction`,
`context`, the five `*_def_scopes` authoring gates and eighteen more never
reached the client. Rather than add the missing 27, the mirror is deleted: a
converter makes the authoritative struct the wire shape, so there is no third
thing left to fall behind. And the renderer showed no capability gate at all, not
even `memory_scopes`, so even served none of it would have appeared — an agent
granted the tenant plane looked identical to one confined to its own scope.

The guard is two assertions because they catch different faults. **Coverage**
populates every field and requires none left at zero, which catches the forgotten
assignment. The **round trip** requires equality, which catches a field wired to
the *wrong* source — `SqlScopes: def.MemoryScopes` leaves nothing zero and is
still wrong. A round trip alone would also miss a symmetric omission, since a
field dropped by both directions survives it untouched. Both faults were verified
by injection rather than argued.

**Tenant focus on the client's memory admin methods (#1128).**
`@loomcycle/client` 1.72.0: the five memory browse methods take an optional
`tenant`. `listMemoryScopes` deliberately does not. A blank focus is dropped
rather than sent, because the Web UI's switcher is empty until an admin types one
and empty there means "my own tenant", not a tenant named `""`.

### Known state, stated plainly

**This release closes a chain that began with ontology-declared memory
placement.** v1.71.0 made placement reachable at all — the shipped consolidator
had held no `sql_scopes`, so neither half of a placed fact could reach the tenant
plane — and v1.71.1 fixed placed facts arriving **orphaned** from their subjects,
because the `about` edge joining them was written in the caller's scope where
neither endpoint existed. Neither has its own section here: patch releases carry
their notes on the annotated tag.

**Placement has now been measured.** On a two-user corpus, duplication fell from
30 duplicate copies to 23 — cross-user duplication was eliminated and *replaced*
by tenant↔owner duplication, so the honest figure is −23%, not elimination. The
real payoff is sharing: **94 facts are readable by both users where 0 were
before**. A second-order effect was unpredicted — cross-user type stability, with
`subject types held stable` at 4 for the user who wrote first and **91** for the
second, whose extractor kept proposing new types for subjects already on file in
the shared plane.

**The self-guard cannot protect what the counterparty also recorded.** Each user
played one speaker in the corpus. The guard correctly kept a speaker's own facts
in their scope, while the *other* user, for whom that speaker is a third party,
published the same facts to the tenant plane. So placement left the owner's facts
more exposed than baseline, which is precisely what the guard exists to prevent.
Not a bug — the consequence of per-scope decisions with no global view. In any
multi-party corpus the guard's promise is defeated by the other party, and the
narrow declarations (`organization`, `project`, `service`) are the safer default.

## What's in v1.71.0

Two lines land together: the layered-context work reaches the multi-agent case, and
ontology-declared memory placement becomes reachable at all.

**A stateful sub-agent hands its state up, not its transcript (#1122).** When a fan-out
parent spawns a stateful child, the parent now receives the child's final structured
state Σ as the result rather than only its prose. A single spawn folds Σ into the
child's `tool_result` as JSON; `parallel_spawn` carries it as a structured `state` field
on each envelope entry; the spawn ledger captures it so a parent restored from a
snapshot keeps a completed child's Σ. That turns N growing transcripts into N compact
structured results — `O(T²)`-per-agent × N becomes N independent `O(T)`. Non-stateful
children are unchanged, and the Team orchestrator is string-only end to end, so
carrying Σ there is still a follow-on.

**`context.mode: auto` routes by the model that actually resolved (#1120).** An
operator sets `auto` once and the agent runs schema-free **recap** on a local backend
and structured **stateful** on a frontier API, instead of hand-picking per deployment —
because structured state needs reliable structured output and a weaker local model
cannot be relied on for it. Providers gained a `Local` capability to make that a routing
fact rather than a guess (`ollama-local`, vllm and llamacpp are local; the hosted
`ollama` is not). The mode resolves once at run start on a clone of the context, so the
shared agent def is never mutated; an interactive run never resolves to stateful, since
that loop has no steer/park. An explicit mode still wins, and an agent with no context
block stays `append` — byte-identical, so `auto` is opt-in. The same PR closed a
transport gap: the per-run `context` override now flows through the connector and MCP
`spawn_run`, matching `sampling` and `compaction`, which were already there.

**A model may propose a state schema; only an operator adopts it (#1121).** A stateful
agent can suggest the shape its task's state should hold via `emit_state`'s
`propose_schema`. The proposal is inert — recorded on the transcript, returned on
`RunResult.ProposedSchema`, surfaced only when it differs from the active schema — and
changes validation for no run. Adoption reuses the versioned agent-def substrate: an
operator forks the def with the schema in `context.state_schema` and promotes it. Same
fail-safe as the ontology's propose→adopt, and no bespoke store for it.

**Ontology-declared placement was unreachable, in every deployment (#1123).** v1.68.0
shipped the mechanism and v1.70.0 fixed the extractor that was starving it, but nothing
could place a fact: the shipped consolidator held `memory_scopes: [agent, user]` and no
`sql_scopes` at all. A placed fact is stored **twice** — a k/v row, which `recall`
searches, and a typed chunk, which a graph walk reaches — so `tenant` has to be on both
grants or the two halves cannot land in the same scope. The bundle now grants them.

The absence had been deliberate, on the argument that an unused grant is the capability
an injected instruction reaches for. That argument is written for a *prompt-driven*
agent, where model output steers control flow. The consolidator is `provider: code-js`:
a deterministic body in which model output is data that gets written and never code that
gets run. What bounds the tenant write is the placement resolver, which fails closed on
every uncertainty — an unknown or inconsistently-typed subject, or a fact about the
profile owner, stays with the user. The grant is also inert on its own: no shipped
ontology type declares a scope, so nothing is placed until an operator declares one for
a type, per tenant. Placement remains **off** by default.

**Both ways to change a bundled agent's grants were broken, and finding out cost an
instance (#1123).** A runtime `AgentDef fork` to flip one grant returned
`fork: definition (139169 bytes) exceeds max 131072` — for a code body the overlay never
touched. `MaxCodeBytes` (256 KiB) exists precisely so executable source is not judged by
the whole-definition cap (128 KiB), but the definition check measured the JSON with the
body inside it, so any body between the two passed the cap written for it and was
refused by the smaller one. The consolidator's ~133 KB sits in that dead zone; the
dedicated cap could never bind. It now measures the definition without the body, on
create and fork alike, and the stored definition is unchanged.

The config route merges correctly — a re-declared bundle agent keeps its body, provider,
tools and gates — but layered onto a stack that does *not* contain the bundle, the same
overlay is indistinguishable from a new agent, so validation refuses it with
`no model, no tier, and no defaults.model`. Accurate, and it sends the reader to inspect
the overlay they just wrote rather than their `LOOMCYCLE_PRESETS`; boot is fatal, so the
wrong first guess costs a down instance. A declaration carrying nothing but capability
grants now says so and names the layer stack to check. Two statements of the `sql_scopes`
enum that listed `agent, user, run` long after `tenant` was added — one of them the
validation error itself, so a *correct* config read as rejected — are fixed, and the
message is now rendered from the set.

### Known state, stated plainly

**Whether declared placement is worth having is still unmeasured.** It is now reachable
for the first time, which is the precondition, not the result. The baseline it has to
beat was measured on a two-user corpus before placement could fire: **30 duplicated
facts, 22%** of the store.

**The `sql_scopes` grant is wider than this needs.** The gate consults a single list, so
opening the Document tenant-chunk path opens the raw `Memory sql_exec` surface on the
tenant keyspace with it; there is no narrower grant today. The consolidator body issues
no SQL op, and a test now asserts it does not, so adding one means re-arguing the grant.

**A failed fork can leave an orphan def row.** `AgentDefCreate` bootstraps a v1 row from
static config before the cap check runs, and does not set the active pointer — so
nothing serves it and resolution still falls through to static config. Do not `promote`
such a row: it captured the pre-change definition.

The adapters and protos are unchanged and remain at 1.67.0, so no `python-v` tag
accompanies this release.

## What's in v1.70.0

*(This entry was reconstructed from the annotated tag, which is authoritative — the
release was cut without a `REVISIONS.md` section.)*

**The extractor is told who the owner is, or told nothing (#1118).** v1.68.1 gave the
extraction prompt a rule worse than no rule, and a live two-speaker run proved it: it
said *"a fact about THEM takes the subject `user`"* and never said who THEM was, because
the declared Identity names live in the user-root document, which reaches
`{{memory:user_info}}` and not the extractor. So the model guessed the more prominent
speaker. In a scope whose profile declared Dave, it made `user` = Calvin — **inverting
the self-guard**, protecting Calvin's facts as if they were the owner's while leaving
Dave's, the actual owner's, placeable. Protecting the wrong person while exposing the
right one is worse than doing neither. `Context op=self` now reports `self_names`,
parsed server-side by the one function that owns the column-0 rule that stops the
template's own indented example from naming every unedited profile after it. The names
are **omitted**, not empty, when nobody declared any, so a caller can tell "nobody said"
from "said nothing"; the prompt then asks only for consistent spelling rather than
inviting the model to identify an owner it cannot know.

**Layered execution context (#1117, #1119).** L1 reasoning-recap context retention and
L2 structured execution state — the first two phases, with an RFC 7386 merge plus a
minimal schema validator for the structured-state layer.

**What the measurement found.** On the only real corpus available, the local extractor
produced **zero** facts from 568 turns while a cloud model produced **74** from the same
input — so the earlier "typing is inconsistent" finding had been measuring a model that
was barely functioning, not a design defect. With a capable extractor the typing bars
pass: 62/62 linkage, 0% of claims on a multiply-typed subject, repeat-subject
consistency 1.00 against 0.00 before.

## What's in v1.69.0

v1.68.0 shipped ontology-declared placement. A two-user run on a real corpus then
showed it could not work: **every typed fact was being lost before it reached the
graph.** This release is what that measurement found, and it is worth reading in
that order — none of it was predicted from the code.

**A subject keeps the type it is already filed under.** A subject node's natural key
is `type + ":" + slug`, so the type is part of the identity. Each extraction call
picked a type fresh, with no knowledge of how that subject was typed before, and
nothing reconciled them. On a 142-claim benchmark store one person existed **five
times** — `event`, `location`, `object`, `organization`, `person` — carrying 89 claims
across five nodes, and **94% of all claims hung off a multiply-typed subject**. So
*"what else do we know about her"*, the question the entity tier exists to answer,
could reach at most a fifth of what was stored.

The type a subject already has now wins over the current call's guess, for both the
key and the field. **First write wins**, and that is the design rather than a
tie-break: it is the only *stable* choice, because any rule that can change a
subject's type later re-partitions every fact already filed under the old one. A
wrong-but-stable type keeps one subject's facts together, which is what matters.
Existing multiply-typed nodes are **not** migrated — this prevents new splits.

**An undeclared type becomes a candidate instead of a lost write.** The extractor
invents kinds the tenant has not declared — `experience`, on the corpus measured — and
`upsert_chunk` refused those writes, so the fact landed in key/value with no graph
presence at all. The gate's own message is that an undeclared-type node *"becomes a
node nobody can find"*; refusing produces **no** node, which loses the subject
entirely.

Such a type is now filed as an **inert ontology candidate** and the subject written
under `object`. The candidate changes nothing for any run until an operator accepts
it, so an invented kind becomes something a person can adopt rather than a silent
loss. A statement class misused as a kind — `preference:user` reads as *"the entity of
type preference named user"* — also falls back rather than being dropped, but is not
proposed: those names are already declared and are simply being misused. The key moves
with the type, because the type is identity; and the retry is matched against the
gate's own wording, so a store fault is never mistaken for an undeclared type.

**A failing consolidation pass says why.** The consolidator counted its graph failures
and discarded the reason — and there is no second place to look, because it is an
`internal: true` agent whose runs are kept out of the run and history surfaces and
whose transcript cannot be read back. The pass report is the only thing it ever gets to
say. It now carries the first error text, and counts a failed subject-type **lookup**
separately from a failed chunk **write**: one counter for both is what made a live
signal uninterpretable.

**Identity documents on every user-creation path.** v1.68.0 provisioned the user-root
and tenant-root documents when a principal was established — for one of the three paths
that establish one. The Web UI drives the other two, so users created there got no
profile: no Identity section, no way to declare their own names, and placement cannot
tell a fact about that person from a fact about a colleague without them.

### Known state, stated plainly

**RFC CQ's gate is still not satisfied.** On the only real corpus available, 1,136
turns of input across two users produced **four facts**, with 35–39 malformed extractor
replies dropped per pass. Placement cannot matter at that rate, and a
typing-consistency number computed over a handful of subjects is not a result — that
mistake has already been made once in this line and corrected. The yield question is
what gates a real answer, and it is open.

The adapters are unchanged and remain at **1.67.0**.

## What's in v1.68.0

**Organisation knowledge has somewhere to live (RFC CQ).** Some of what a
consolidator learns is not about the user — *"the checkout-api service requires two
approvals"* is true for everyone in the tenant. Until now every fact went into one
user's scope, so a colleague either re-learned it from their own conversations or
never learned it at all, and there was no surface anywhere that would tell an
operator this was happening.

**Placement is operator config, not per-fact inference.** An entity type in the
tenant ontology may declare which memory scope facts about that kind of thing belong
in:

```
## service
- `@memory_scope` tenant
- `name` — what people call it
```

A fact reaches its scope through the subject entity it is already linked to. The
alternative — asking a model per fact — would put a judgement in front of every
write, thousands of independent chances to put a private sentence in front of the
whole tenant, and it would land on the extractor, whose own prompt is documented as
*"a mitigation, not a guarantee"*. A handful of declarations, authored once and
versioned in a document, replaces all of that. It also costs nothing at write time:
the type is already assigned.

**Every uncertainty declines to move the fact**, and that asymmetry is the design
rather than caution. Declining costs exactly what the system already costs — the fact
stays in one user's scope. Moving one wrongly is not recoverable. So a placement is
refused for an undeclared or unknown type, a draft ontology, no ontology at all, a
subject that names the run's own user, a subject the store types inconsistently, an
isolated member, and a scope the writing agent has not been granted — each with a
reason an operator can act on.

**Both halves of a fact move together, or neither does.** A fact is stored twice: the
key/value row semantic recall searches, and a chunk mirror the graph walks. Split
those across scopes and `recall` finds the fact in one place while `graph_recall`
finds it in another, which is worse than never moving it. So the decision is made
once per batch, before either write, by the writer that owns both — a new read-only
`Memory op=placement`. A tenant placement consequently needs the tenant grant on
*both* `memory_scopes` and `sql_scopes`, because the mirror is a Document write.

**A user can say who they are.** The per-user profile document gained an Identity
section — `@name` and `@alias` bullets — because nothing else can tell a fact about
the user under their own name from a fact about a colleague: *"Ada prefers Go"* and
*"Maria owns the release process"* are the same shape. With names declared, the
user's own facts stay theirs whatever their type says. Undeclared, that gap remains
exactly as it was, which the test suite asserts out loud rather than assuming away.

**The identity documents now exist when a principal does**, at token mint and at boot
for config-declared principals, instead of appearing on the first run that happened
to reference them. A template that arrives after the moment it was needed is not a
template. `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS=0` restores the old lazy-only
behaviour.

**Reads are unchanged, and now say so.** A memory read touches exactly one scope —
it always did, but the `scope` description described *who* could reach the tenant
keyspace and never that a separate call is required to read it, so an agent asked
once, got nothing, and concluded the organisation knew nothing. Both `Memory` and
`Document` now state the invariant and the remedy: one scope per call, two calls to
consult two, merge by score.

**Inert until an operator turns it on.** Nothing is declared out of the box, the
consolidator's grants are untouched, and a deployment that edits nothing behaves
exactly as it did in v1.67.0.

### Also in this release

**A sqlite upgrade blocker.** Any deployment whose `memory` table predated RFC CL
(v1.65.0) could not start on v1.66+: `migrate` created three partial indexes ahead of
the `ALTER`s adding the columns they name, and on an existing table
`CREATE TABLE IF NOT EXISTS` is a no-op, so the index failed on a missing column and
the store never opened. A fresh database gets those columns from `CREATE TABLE`, which
is why every test passed — they all start from an empty file. Both halves are now
tested: a legacy-shaped fixture, and a guard that reads the two statement lists and
fails for any table.

**Wikidata benchmark harnesses** for knowledge updates, ontology typing and
cross-lingual recall, plus a bulk fact-corpus builder and importer.

### Known state, stated plainly

RFC CQ's own gate — whether a real store's entity typing is consistent enough to
carry a scope decision — **has not been run on a real multi-user store**, and its
preliminary reading on a small development scope *fails*: linkage was complete, but
two of five subjects carried two types, and one of them would place a personal fact
in the shared plane. That is a data-quality problem rather than a design one, and it
is why placement declines an inconsistently typed subject instead of guessing. Until
that measurement exists, treat a `@memory_scope` declaration as something to try on a
store whose typing you have looked at.

The adapters are unchanged in this release and remain at **1.67.0**.

## What's in v1.67.0

**Per-user credential self-service (RFC CN).** A logged-in user can now store its
OWN API tokens — a personal Slack/Telegram bot token, a per-user webhook secret —
without handing the secret to a tenant operator. The *consumption* side already
bound per-user credentials (`$cred:<name>` resolves **agent > user > tenant**, per
run); the missing half was letting a user *put a token in*. Now every transport does.

Before this, `POST /v1/_credentialdef` was `substrate:tenant`-gated: an isolated
`substrate:user` user was refused at the tenant-member isolation floor, and the Web
UI's only credential control lived in the operator Settings hub. The rule now: a user
may manage **only `scope=user`** credentials keyed on its own subject; an omitted
scope defaults to `user`; `scope=tenant`/`agent` still requires `substrate:tenant`.
One shared check (`credential.ConstrainToUserScope`) enforces it identically on every
surface, so the transports cannot drift.

### Across every transport

- **HTTP** (#1093) — `POST /v1/_credentialdef` admits an isolated user (additively —
  tenant/admin authoring and the RFC CB member path are unchanged), confined to
  `scope=user` by the handler.
- **MCP** (#1095) — an isolated `substrate:user` session may open `/v1/_mcp` and call
  **only** the `credentialdef` meta-tool (the `loomcycle mcp --upstream` thin client).
- **Web UI** (#1096) — a standalone **"My Credentials"** page, visible to every login
  (including a delegated user with no Settings gear); the operator Settings →
  Credentials tab (tenant authoring) is unchanged.
- **gRPC + adapters** (#1097) — a new `CredentialDef` RPC, plus
  `createCredential`/`listCredentials`/`deleteCredential` on `@loomcycle/client` and
  `credential_def()` on the Python client. Both adapters ship at **1.67.0**.

The worked example — each user receives its own GitHub webhook and publishes to *its*
Slack/Telegram channel with *its* bot token — needs no per-user def authoring: an
operator authors the flow once referencing `$cred:telegram`, and each user self-serves
only its token, which wins per run over the tenant fallback.

### Also

- **fix(history):** `op=recap` now writes a **short** chat-list summary (at most two
  sentences, under 256 chars) instead of reusing the compaction prompt — which on a
  long chat returned several paragraphs that no list surface could rely on (#1092).
- **bench:** the LoCoMo harness gains an **answer axis** (`-mode answer` — whether an
  agent can *answer* from what it retrieved, not just whether the right rows come
  back) plus the RFC CL/CM measurement instrumentation (#1094).
- Refreshed the default local-provider model aliases in the `local` preset.

## What's in v1.66.0

Two feature lines land together: **RFC CK** makes local inference a first-class
bundle and lets an operator reload a running config without dropping in-flight runs,
and **RFC CL** gives a memory row a sense of *time* — when a thing was said and when
it was true — so a question can ask about a moment, not just a topic. Minor rather
than a patch: both add runtime primitives the binary must carry, and the Memory tool
plus the adapters gain surface.

### RFC CK — local providers in bundle YAML, and on-the-fly config reload

**Local inference is now configured in YAML, not scattered across ENV.** Dedicated
`vllm` and `llamacpp` drivers (OpenAI-compatible, on the DeepSeek delegate pattern)
join `ollama-local`, and the whole local-provider matrix — `base_url`, the advertised
context window, header/idle timeouts, enablement — is settable in a bundle's
`providers:` block, with `loomcycle.yaml` and ENV still overriding per the existing
layer order (#1079).

**`POST /v1/_config/reload` reloads a changed config in place** — the retune that used
to mean a restart (a bigger `num_ctx`, a new endpoint, a longer timeout) now takes
effect without dropping in-flight runs. It re-assembles the same layered stack the
server booted from, **validates the candidate before applying** (a typo is rejected
`422` and the running config keeps serving), applies the sections it can apply live,
and reports the rest under `restart_required`; `?dry_run=1` returns the section diff
without applying.

- The endpoint + in-place resolver/provider rebuild (#1080), a config `Holder` that
  makes `user_tiers` / `agents` / `defaults` reload live (#1081), and subsystem
  reloaders for concurrency caps, `scheduled_runs`, and channels (#1086).
- **Section-per-file config**: a `loomcycle.yaml` base auto-layers its
  `loomcycle.*.yaml` siblings (deep-merged, lexical), so a large config splits by
  section — `loomcycle.providers.yaml`, `loomcycle.memory.yaml`, … (#1083, #1084).
- **Re-glob on reload**: adding or removing a config or section file after boot is
  picked up on the next reload — no restart (#1088).
- Deliberately restart-required, with reasons reported: the memory embedder (a
  model/dimension change invalidates every stored vector), skills, the listen
  address, and the store DSN.

### RFC CL — a memory row learns *when*

A key/value memory row used to carry one time — `created_at`, when loomcycle stored
it, which on a bulk import is one clustered instant for the whole corpus and answers
none of the questions people actually ask. It now carries three:

- **`observed_at`** — when the thing was *said or written* (#1085). A new **`when`**
  predicate on `search` / `recall` narrows by it, and `set` takes it to date a row.
  Caller-supplied and never inferred — a guessed date is worse than none, because it
  silently filters the right row out of a window it belongs in. Soft by default:
  undated rows survive the window for the ranker to demote (`missing: prefer`).
- **`valid_at` / `invalid_at`** — when the thing was *true* (#1087). An **`as_of`**
  predicate answers "what was true on the 3rd" exactly, where the observed window
  could only approximate it. `as_of` is always a hard filter — a row valid over a
  different interval is not a weaker answer, it is a wrong one. Half-open
  `[valid_at, invalid_at)` with NULL meaning still true.
- The consolidator now **dates the durable facts it already keeps** (#1089), so
  `as_of` can answer "what medication was I on in April". Scoped honestly to semantic
  (durable-state) memory — it does not attempt episodic "what did I do that day"
  coverage, which belongs in its own RFC.

The adapters gain the memory-search time surface: **`@loomcycle/client` 1.66.0** and
the **Python `loomcycle` 1.66.0**.

### Also

- **fix(store):** a Postgres def-INSERT placeholder-count regression (landed with the
  phase-2a memory change) failed every `agent_def` / `skill_def` / …`_def` create on
  the Postgres store; the def INSERTs are restored to their correct column count
  (#1090). The sqlite path was unaffected.

## What's in v1.65.0

**`recall` told the model every row was a fact, and carried no kind.** Minor rather
than a patch for the same two reasons as v1.64.0: the fix is in the runtime binary,
which a `vX.Y.Z` patch tag does not build, and the shape of `recall`'s result changes
— the array is renamed and each row gains a field.

### It promised a `kind` and never sent one (#1077)

The Memory tool's input schema has always said, of `sources`, that *"each result
carries a matching kind"*. For `search` that is true. For `recall` it was not: the
projection rendered `{id, memory, score}` and nothing else, so a caller could not
tell a consolidator-distilled fact from a remark an agent had jotted down from
document prose. The class was already being computed — the source selector filters on
it — so nothing was missing but carrying it out to the caller.

This is the same defect class as v1.64.0's, which added `kind` to `search` and left
`recall` alone.

An **empty kind stays empty**. A remote memory layer handing back opaque server-side
ids cannot classify its own rows, and defaulting those to `"fact"` would recreate the
very thing being fixed, so the field is omitted instead.

### And the array called them all facts anyway

Recall's default admits notes as well as facts, and on a corpus of raw ingested turns
EVERY row is a note — so `facts[]` asserted, of every result, a status most of them
did not have.

Not cosmetic. The LoCoMo answer axis run twice over one corpus (1,535 questions, same
judge, same answerer model, 1,524 graded by both), changing only which op the answerer
called:

| slice | `op=search` | `op=recall` | delta |
|---|---|---|---|
| **overall** | **0.6906** | **0.6692** | **-0.0214** |
| single-hop | 0.7873 | 0.7873 | 0.0000 |
| multi-hop | 0.5344 | 0.5283 | -0.0060 |
| temporal | 0.6589 | 0.5727 | **-0.0862** |
| open-domain | 0.3571 | 0.3258 | -0.0313 |

**Retrieval was identical between the two.** Probed in-band with the same query, both
ops returned the same ten keys, in the same order, with the same scores — expected
here, because with no consolidation pass every row is a note, so recall's facts+notes
default and search's unfiltered scan select the same set (`facts_written=0` in both
runs). The entire gap is the projection the model reads.

It concentrates in temporal questions, and the failure has one shape: of 43 temporal
answers that went correct to wrong, 29 reported the timestamp of the UTTERANCE as the
date of the EVENT.

```
"When did Caroline go to the LGBTQ support group?"    gold: 7 May 2023
  through search:  "7 May 2023 (she went 'yesterday' on 8 May)"   correct
  through recall:  "8 May 2023"                                   wrong
```

Reading a dated remark as a standing fact is what "facts" invites. The other 14 were
abstentions (NOT_FOUND 0.134 → 0.167).

So the array is now `memories`, each row carries its `kind`, and the op's description
says what a remembered remark IS: something recorded, not something established, often
stamped with the time it was SAID rather than the time it happened.

### Upgrade note

`recall` returns `{"memories": [{id, memory, score, kind}]}` where it previously
returned `{"facts": [{id, memory, score}]}`. Anything reading `.facts` off a recall
result needs `.memories`.

There is deliberately **no compatibility alias on the wire**: this is model-visible
tool output, and emitting both names would leave the misleading one in front of the
model, which is the whole point of the change. The one in-tree consumer — the code-js
consolidator's `recall()` — reads `memories || facts`, because a code-js body replays
against tool results recorded earlier in the SAME run, so a pass straddling the
upgrade would otherwise read undefined, recall nothing, and write a duplicate instead
of merging.

`Document op=list_facts` is untouched and still returns `facts` — those are facts.

### Measured after the fact — it held

The figures above are from the OLD projection; this section originally said the
recovery was a hypothesis, not a result. It has since been run. Same corpus, same
answerer def, same judge, same model — only the projection differs:

| slice | `op=search` | recall (old) | **recall (v1.65.0)** | vs old | vs search |
|---|---|---|---|---|---|
| **overall** | 0.6906 | 0.6692 | **0.6915** | **+0.0224** | +0.0009 |
| single-hop | 0.7873 | 0.7873 | 0.7860 | -0.0013 | -0.0013 |
| multi-hop | 0.5344 | 0.5283 | 0.5356 | +0.0073 | +0.0012 |
| **temporal** | 0.6589 | 0.5727 | **0.6701** | **+0.0974** | +0.0112 |
| open-domain | 0.3571 | 0.3258 | 0.3678 | +0.0420 | +0.0107 |

1,535 questions, 1,454 graded. `recall` went from 2.2pp behind `search` to level with
it, and every category is now within about one question of `search` — the gap is
closed, not narrowed.

The recovery landed where the diagnosis said it would. Temporal gained 9.7pp;
single-hop, which does not depend on resolving a date against an utterance time,
stayed flat at -0.13pp. A uniform lift across categories would have been WEAKER
evidence — it would have suggested some general effect rather than the specific
mechanism claimed. Of the 43 temporal questions that `search` answered correctly and
the old projection got wrong, 30 are now correct, 12 still wrong, 1 unparsed.
Abstentions fell 0.167 to 0.158.

### What the remaining 12 are, and they are not this bug

Investigated, because "mostly fixed" is not a finding. They split in two, neither of
which the projection can reach:

**Five are the deictic step failing at the last inch.** The model now demonstrably
UNDERSTANDS the offset and still answers the wrong date — one reply reads *"8 May 2023
(she went 'yesterday,' stated 8 May)"*, which states the correct reasoning and then
emits the speaking date anyway. Nothing about the row's framing is misleading it any
more; it is an instruction-following failure at the final-answer step, and it belongs
to whatever prompt is asking the question.

**Seven need a time PREDICATE, which this memory plane does not have.** They ask
things like *"which city was Calvin at on October 3, 2023"* or *"what was Dave doing
in the first weekend of October 2023"*. Cosine similarity over prose cannot answer
that: the embedding of a date-constrained question does not retrieve the turn that
happens to carry that timestamp, because dates are not semantically encoded. Five
abstained and two answered confidently wrong, which is the worse failure.

That splits cleanly in the aggregate, across all 294 graded temporal questions and not
just the 12:

| temporal question shape | n | accuracy | abstention |
|---|---|---|---|
| carries an absolute date / window constraint | 31 | 0.5484 | 0.323 |
| topic-shaped | 263 | 0.6844 | 0.171 |

So a date-constrained question is roughly 14pp harder and abstains about twice as
often. The machinery for it already exists elsewhere in the runtime — the bi-temporal
entity sidecar's `valid_at` / `invalid_at` and `graph_recall`'s `as_of` predicate — but
the k/v memory plane's `recall` and `search` expose no date filter at all. Closing
that is a feature, not a fix, and wants its own RFC.

## What's in v1.64.0

**`recall` could not see an agent's own notes.** Minor rather than a patch for two
reasons: the fix is in the runtime binary, which a `vX.Y.Z` patch tag does not
build, and a bare `recall` now returns notes alongside facts — a visible behaviour
change for anything reading it.

### Two defects, either of which alone empties the result (#1075)

`Memory op=recall` returned NOTHING on a scope holding 419 embedded rows, while
`op=search` returned three hits for the same query in the same partition:

```
search sources=[notes]        → 3 results (cosine 0.546, 0.539…)
recall sources=[notes]        → 0
recall sources=[facts,notes]  → 0
recall (no sources)           → 0
```

**The in-band parser silently dropped `notes`.** `parseSources` had cases for
facts and documents and none for notes — while the op's OWN input-schema enum
advertises all three. Unknown values are dropped rather than rejected by design
(rejecting would break an older runtime against a newer value name, which is the
right call), so an explicit `sources:["notes"]` became NO selector. One cause, two
opposite symptoms: `search` widened to everything and looked correct, `recall`
fell through to its default and returned nothing. The HTTP parser
(`parseMemorySources`) had always handled notes, so the same selector meant
different things depending on which surface a caller used.

**Recall's default excluded notes, contradicting its own schema.**
`inprocess.Recall` defaulted to facts alone; the schema promises "facts+notes". A
row written with `set` — or with an off-run PUT — carries no provenance, so
`ClassifyMemoryRow` calls it a NOTE, and the default hid every one of them. The
facts/notes split landed after that default was written and the line was never
revisited: before the split, "facts" WAS the whole of an agent's own memory.
Documents stay excluded, which is the separate and still-correct reason the
default exists at all (a horizontal rule outranking the fact holding an answer).

The `SourceFacts` doc comment still described facts as including notes; reading it
that way is what made the default look intentional. Corrected.

### How it surfaced, and what it was costing

The LoCoMo answer axis. An answerer whose prompt says `op=recall` scored **0.0000
with a 94% abstention rate** over 419 embedded conversation turns — it was finding
nothing, not answering wrongly. Pointed at `op=search`, which by accident of the
first defect applied no filter at all, the same store, same embedder and same
questions scored **0.7353 with zero abstentions**.

So the practical cost was: any agent following the documented advice to read its
own memory with `recall` saw only what a consolidator had distilled, and nothing
it had written itself.

### The fixture could not have caught it, which is also fixed

The inprocess test double honoured only `filter.KeyPrefix`. It ignored
`filter.Provenance` and `filter.ExcludeKeyPrefix` and never populated
`MemorySearchEntry.Origin`, so NO test in that package could distinguish a fact
from a note or exclude a document — source filtering was structurally untestable
there. The double now records each row's origin on the way in (the real stores
keep it in a column `MemoryEntry` does not expose, so a double reading rows back
cannot recover it), applies both filter dimensions, and carries `Origin` through.

That mattered immediately: the fail-before for the second defect initially PASSED,
because the assertion could not observe the thing it named. Making the double
faithful is what turned it into a regression test.

### Adapters

None. The fix is runtime-internal, so `@loomcycle/client` and the Python package
stay at 1.61.0 and `@loomcycle/library` at 0.3.0.

## What's in v1.63.0

**A curator that never ran, and the two agent fields you could not set without
curl.** Minor: the changes are in the runtime binary and the embedded Web UI,
neither of which a `vX.Y.Z` patch tag builds.

### memory/ontologist named its own tool in lowercase and never ran (#1072)

It failed its FIRST tool call on every run, then spent the rest of its budget
reasoning about which tools it had. A live pass: `tool not found: document`,
followed by 1,937 output tokens concluding — wrongly — that the Document tool was
missing from its environment, and inventing two tools
(`generate_tool_suggestion`, `search`) that exist nowhere in loomcycle.

The def was fine. It grants `tools: [Document]`. The PROMPT told the model to call
`document` — lowercase, three times — and tool dispatch is an exact-match map
lookup (`tools.Dispatcher.Execute` does `d.tools[name]`), so the call never
reached the tool and never could.

Nothing caught it because the two halves are checked by different things and
neither checks the pair: the ACL is validated at config load, the prose is not
validated at all. The agent looked correctly configured in every listing, and the
only symptom was a curator producing confident nonsense.

`TestBundlePrompts_NameToolsExactly` now scans every bundle agent's system prompt
for the backticked `` `name` op=… `` form the bundles use to teach a tool call and
asserts the name is one that agent is granted. Scoped to that form rather than to
every mention of a word, because "ordinary document chunks" is legitimate prose in
the same paragraph — and it fails when the pattern matches nothing, so it cannot
pass by examining zero instructions.

### The agent modal can set max_context_tokens and internal (#1073)

Both round-trip the AgentDef create/fork overlay and neither was reachable from
the Library modal, so setting either meant hand-rolling an API call.

`max_context_tokens` sits one row from `max_tokens` and they are trivially
confusable with different consequences — one truncates the reply, the other
truncates the prompt — so both inputs carry a hint. The hint leads with what
decides whether you want it: on a local model this becomes THAT agent's own
`num_ctx`, so a smaller window is a cheaper and faster call; on a cloud model it
can only lower the effective window, never raise it.

`internal` is a checkbox that says ONE-WAY, because `applyOverlay` does
`if ov.Internal { d.Internal = true }`. A plain checkbox would let an operator
untick it, fork, and get an agent that is still internal with nothing saying so.
The overlay emits the key only when true, mirroring that merge instead of sending
a `false` the server ignores.

Also fixed the text that hid both: the overlay's own schema `description` listed
neither, so an MCP caller reading the tool schema could not discover them either.
That description still documents only 24 of the overlay's 46 fields — the 22 it
omits include `sampling`, `compaction`, `volumes`, `sql_scopes`,
`memory_consolidation` and the four `*_def_scopes` gates. Left for its own change,
because a coverage test would fail on twenty fields unrelated to this one.

### Packages

`@loomcycle/library` 0.3.0 (the modal change), published from its own
`library-v0.3.0` tag. The Web UI embedded in the binary compiles that source
directly, so the runtime carries the change either way. `@loomcycle/client` and
the Python adapter are unchanged at 1.61.0.

## What's in v1.62.0

**Three ways a consolidation pass wasted a deployment's time, all found by running
one.** Minor: the changes are in the runtime binary and the embedded bundle, neither
of which a `vX.Y.Z` patch tag builds. No adapter or wire change.

The occasion was a LoCoMo memory benchmark on a local-model deployment. Every item
below is measured from that run's event log rather than reasoned about.

### A killed pass no longer strands its lease (#1069)

The consolidation pass releases its lease in a `finally`, which covers every path
its own code can take — and none of the paths where the **run** is killed out from
under it. The owner of a lease is the run id and `cursor_release` is
ownership-scoped, so a dead run can never hand its lease back and nothing else is
permitted to. The target stayed leased until the TTL elapsed.

What that looked like: a pass took the lease at 14:21:46 and spawned an extractor
child that never returned — neither run emitted a `done` event in the following two
hours. The parent hung on that one call, exhausted its 25-minute
`run_timeout_seconds`, and was killed before reaching the `finally`; its 30-minute
lease then sat until 14:51:46. The harness polls every three minutes, so **ten
consecutive passes** bailed with "target busy, nothing read, nothing written" across
a window where nothing was running at all.

Note the shape: `run_timeout_seconds` is deliberately *below* `lease_ttl_ms` so a
live pass can never be stolen from — which means a killed pass was *guaranteed* to
leave a stranded lease, every time.

`MemoryCursorReleaseByOwner(owner)` now frees a run's lease from
`finishRunWithCancel`, the terminal path for every run, as a deferred best-effort
hook. Owner-only with no tenant argument, because a run id is globally unique and a
dead run cannot say which target it had leased. An empty owner is a no-op, since
`''` is what an *unleased* row stores in `leased_by`. The TTL keeps its job as the
backstop for a process that dies without reaching any cleanup; what is gone is the
case where the runtime *knew* the run was over and let the lease sit anyway.

### The queue no longer takes a whole pass per batch, and one bad item stops blocking the rest (#1070)

`consolidatePending` took exactly one extractor call's worth of queued items and
deferred the remainder, so throughput was **one batch per pass** however long the
queue was — a 419-turn conversation measured ten-to-sixteen passes and about an
hour. The items are independent; nothing about correctness required stopping after
one call, only the absence of a loop. Now bounded by `max_pending_calls` (4),
because a pass still has to finish inside `run_timeout_seconds`.

The second half was a **livelock**, not slowness. An empty extraction left the whole
batch queued — right in itself, because an ack is the one irreversible step here and
an empty reply cannot distinguish "these turns hold nothing durable" from "the model
glitched". But it also meant a batch the extractor keeps answering empty for was
re-drained every pass, spent a call, acked nothing, and blocked everything behind it
forever. Observed live as ten consecutive replies of **two output tokens** with the
queue head never moving, and it is why a pre-ingest drain spent ~70 minutes without
reaching the run's own data.

The fix does **not** ack on empty. An empty multi-item batch narrows to its head (a
multi-item batch does not say which item the model could not read), and a single
item that still yields nothing is **stepped over**: left queued for a later pass
while this pass continues with what is behind it. The blockage is gone, no input is
traded for throughput, and a permanently unreadable item settles at one call per
pass instead of every call forever. It is reported, because a silent step-over looks
exactly like a pass that spent its budget and moved nothing.

Worth recording: the first version of this *did* ack a single item that came back
empty, reasoning that the model had examined it alone — the same evidence the chat
path accepts when it lets an empty chat move the watermark.
`TestConsolidator_EmptyBatchReplyLeavesTheQueuedItemsQueued` refused it, correctly:
a transient extractor glitch would have dropped those turns permanently. Step-over
solves the blocking without that trade, and the test passes unmodified.

### The consolidator's children ask for the window they actually need (#1070)

They ran at whatever window the `ollama-local` registration was pinned to, because
until v1.61.0 that was the only knob. RFC CJ made it per-agent and the Ollama driver
prefers `Request.MaxContextTokens` over the construction-time `num_ctx`, so each
child can now size its own — and on a local deployment the KV cache is allocated per
request, so a smaller window is a cheaper and faster call.

Sized from observed input tokens: **`memory/extractor` 16384** (a real extraction
measured 3,826 input tokens; its prompt budget is `max_part_chars`, 12,000 chars ≈
3–4k tokens, plus the injected ontology), **`memory/judge` and
`memory/conflict-judge` 8192** (715–731 input tokens across a live batch of eight),
**`memory/ontologist` 32768** (it surveys the store rather than reading one supplied
text; a live pass ran 86k across seventeen calls).

The extractor's *window* and its *prompt budget* bound different failures, so raising
`max_part_chars` to fill the window would be wrong — 12,000 is measured against the
model, where a 21,635-char prompt came back empty while 15,684 and 15,599 extracted
cleanly.

### Not changed, deliberately

The `lease_ttl_ms` / `run_timeout_seconds` ratio (30 and 25 minutes against a
four-minute pass) still looks generous. #1069 removes the wedge that made it hurt,
and #1070 makes a pass *longer* — up to four extractor calls — so tightening the
budget now would cause more kills rather than fewer. Worth revisiting once the new
pass duration has been measured.

### Adapters

None. Both changes are runtime-internal, so `@loomcycle/client` and the Python
package stay at 1.61.0.

## What's in v1.61.0

**Size each agent's context window — per agent and per run.** Minor rather than a
patch: the changes land in the runtime binary and the adapters, neither of which a
`vX.Y.Z` patch tag builds. It completes RFC CJ across both phases, adds a
memory-retrieval benchmark harness, and fixes a load-dependent test flake.

### A context window per agent (#1065)

One Ollama host served every local agent the same context window — the global
`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` (or a per-provider `options.num_ctx`), or, unset,
Ollama's silent 4096-token floor. A fetch-heavy `researcher` that needed 128K and a
`chat` agent that wanted 16K could not coexist on one registration without standing
up a second provider.

A per-agent `max_context_tokens` now sizes the window for **that agent only**. On a
local (Ollama) model it is sent as `options.num_ctx`, winning over both the env knob
and any per-provider value; the driver reports the source as `(request)`. On a cloud
model — where the window is fixed by the model — it instead caps the agent's
*effective* window (the compaction budget the context gauge and `autocompact_at_pct`
read), clamped to the model's maximum, so it can only lower, never enlarge. It is
distinct from `max_tokens` (the output cap), content-identifying (a fork that changes
it mints a new `content_sha256`), and flows down the spawn tree. `0`/unset is
byte-identical to before.

### Per-run override, op=self visibility, adapter parity (#1067)

The same knob became a per-**run** override, mirroring the existing per-run
`sampling`/`compaction` overrides exactly: a value on `POST /v1/runs` (or a
continuation), on the gRPC `RunRequest`/`ContinueRequest`, on the MCP `spawn_run` /
`spawn_runs` and the HTTP `/v1/runs:batch` fan-out, and on both adapters. A per-run
value `> 0` wins over the agent's own; `0`/absent inherits it (which itself falls
through to the provider/driver default). Because a window is never meaningfully `0`,
absence and zero coincide — so it is a plain scalar (a new `config.MergeMaxContextTokens`
helper beside `MergeSampling`/`MergeCompaction`) and a plain `int32` on the wire, not
a proto3 `optional`. Sub-agent spawns and resumed runs deliberately use the agent-def
value only — a per-run override is neither inherited by children nor snapshotted, the
same posture per-run sampling takes at those two sites.

An agent can now read its resolved cap for its own run via `Context op=self`
(`max_context_tokens`), reported even before the first turn; the *effective* window
after the first turn continues to appear there as `context.max_tokens`.

One pre-existing gap fell out of this: the MCP streaming-spawn path was silently
dropping per-run `sampling` **and** `compaction` on the way to the run input. It now
carries all three overrides, like the blocking path.

### A memory-retrieval benchmark harness (#1064)

`bench/cmd/locomo` adds a convert / ingest / search / all / purge harness over the
LoCoMo long-conversation QA set, so memory-retrieval quality (recall@k) can be
measured against a fixed corpus rather than by feel — the `qa.evidence` dialogue ids
give free retrieval ground truth. It needs pgvector (the SQLite tier has no vectors),
and the corpus is CC BY-NC, so it is never vendored.

### De-flaked the deferred-channel store contract (#1066)

Two deferred-channel store-contract subtests asserted a message was hidden before its
`visible_at` using a 150 ms window; on a loaded `-race` CI runner the window elapsed
before the assertion ran, producing a spurious "visible too early" failure. The
windows are widened to 2 s. Test-only.

### Adapters

Both bump to **1.61.0**. `@loomcycle/client` gains `maxContextTokens` on the run and
continue options (serialized as `max_context_tokens`); the Python package gains a
`max_context_tokens` argument mapped onto the gRPC request. Additive — existing
callers are unchanged.

## What's in v1.60.0

**A fact that contradicts a stored one is now noticed.** Minor rather than a patch
because the change lives in the runtime binary and the embedded bundle, which a
`vX.Y.Z` patch tag does not build. One feature, and it deliberately writes nothing.

### Conflict detection, report-only (#1062)

A fact that contradicted one already in the store did not retire it. Both persisted,
both came back from recall, and nothing marked either as suspect — the failure a
neutral survey of 2026 memory systems calls *hallucinations of the past* and
attributes specifically to append-only fact stores.

Retirement did exist, but it was reached from exactly **one** place. `applyOne` fills
`supersede_queue` only from near-duplicate collapse, where a neighbour qualifies on
cosine similarity plus subject overlap. So the decision to retire a fact was made
entirely by **similarity**, and nothing anywhere asked whether two claims can both be
true. That cuts both ways: two facts that genuinely conflict but score below the merge
band both survive and both recall, while two near-duplicates that do *not* conflict
risk being collapsed into one.

Now, for each fact written, the neighbours in the "related but distinct" band — at or
above `related_threshold`, below `merge_threshold` — go to a new tool-less judge as
**pairs**, which answers `contradicts` / `independent` / `unclear`.

**It writes nothing.** Every verdict is counted, reported and discarded:

```
conflict candidates 3, judged 3 (1 would be retired as contradicted,
2 independent, 0 unclear) — reporting only, nothing was changed
```

That is the design rather than a limitation. The band has never been measured against
real conflicts, and a detector that retires facts on an unmeasured threshold is the
failure the merge band already taught this pipeline about once. The report says what
enforcement *would* do so an operator can calibrate first, and acting on a verdict
will require **its own key** — a deployment that turns detection on today cannot start
retiring facts on a later upgrade without a second, separate decision.

Turn it on with `memory.consolidation.detect_conflicts: true`. Off by default, read
through the capabilities report like `verify_writes` and the similarity bands, and
bounded at four extra judge calls per pass.

### Three things this needed less of than the design expected

- **No new threshold.** `related_threshold` has been in the operator config *and* the
  capabilities report all along, documented as exactly this band's lower edge — it
  outlived the code that used to read it. That code appended `" (related: …)"` to the
  fact TEXT and was removed because the tails nested into the stored value and
  therefore into the embedding, defeating the very merge band they were meant to
  inform. Its epitaph — *"there is nowhere structured to put the linkage"* — is what
  stopped being true: the entity sidecar carries `invalid_at` / `expired_at` /
  `judged_by`, and supersede records which chunk replaced which.
- **No new wire op.** The write path enforcement will need already exists.
- **One Go field.** `detect_conflicts`, beside `verify_writes`.

### One deliberate asymmetry

The subject-overlap gate is **not** applied to conflict candidates, though the merge
path uses it two lines above. There it stops a destructive merge resting on a single
similarity number, and it earns its place. Here it would defeat the purpose: the
reason to ask a model at all is that lexical overlap misses real conflicts — it scores
**0.000** for a non-Latin paraphrase pair, because the tokenizer only sees `[a-z0-9]`,
and around 0.5 for two different-language facts sharing nothing but a brand name.
Screening candidates with the signal whose blind spots this exists to cover would
inherit every one of them. A test with a Cyrillic neighbour fails if the gate returns.

The **class** refusal does carry over, including the half that refuses a key the pass
cannot parse — an opaque row from a remote backend, or one a user wrote themselves.
Detection writes nothing today, so that half is not load-bearing yet; it is enforced
from the start so enforcement needs no widening later.

### A separate judge agent

`memory/conflict-judge` is a new tool-less agent on the same tier as the entailment
judge, not a second question added to it. That judge's system prompt is deliberately
single-purpose and its tier choice rests on a measured eval; a differently-shaped
question in the same prompt would put that result in doubt every time either question
changed. Two single-purpose agents cost one extra def.

### Adapters

None. The flag is yaml, and the capabilities payload is untyped in both adapters, so
`@loomcycle/client` and the Python package are unchanged at 1.58.0 — as at v1.59.0.

## What's in v1.59.0

**The document viewer surfaces the document id and every chunk id, each with a
copy button.** Minor rather than a patch: the Web UI is embedded in the runtime
binary, and a `vX.Y.Z` patch tag builds only the browser sidecar — so a patch
could not ship a UI change.

### Copy document + chunk ids from the viewer (#1060)

Wiring a document or a specific chunk into an external workflow — n8n over the
MCP/HTTP API — needs the exact `document_id` / chunk id the API expects. The
viewer used those ids only internally (React keys, data-layer calls) and never
displayed them, so there was no way to grab one from the UI.

The `@loomcycle/explorer` document viewer now surfaces them, each with a
click-to-copy button:

- the **document id** in the toolbar,
- the **selected chunk id** in the content head,
- a compact copy on **every chunk-tree row** (faint until the row is hovered or
  selected, so the tree keeps its clean scan-line).

The on-screen id is monospace and ellipsized, but the clipboard always receives
the FULL id — a long UUID displays compactly yet copies completely. The copy
click never selects the chunk or toggles the tree, and a failed copy (an
insecure context with no clipboard API) degrades to a `✗` rather than throwing.

## What's in v1.58.0

**A search over the document store now says where its results came from, and an
embedder migration runs in batches.** Minor rather than a patch: the search response
gains two wire fields, and a `vX.Y.Z` patch tag builds only the browser sidecar —
both changes here live in the runtime binary, so a patch tag could not ship either.

### A document search hit names its document and heading (#1058)

A semantic search that spans the doc store returned prose addressed only by an opaque
`doc.chunk:<32 hex>` key. The id is the right ADDRESS — stable, unique, what
`get_chunk` takes — but it is not an identity. A live query for *"the judge withholds
a fact whose span does not support it"* returned six hits, all six of them the right
document, and nothing in the response said so:

```
doc.chunk:2f51f960973ec940301a4d4aebcf35ff
doc.chunk:f5e7492bc3f2065dad0272c969d8beb0
…
```

Anything wanting to cite a result had to fetch each row just to learn its heading.

**Keying by title instead was the tempting fix and is wrong**: titles are not unique,
not stable under a rename, and not addressable. So the key stays and the hit carries
its readable identity beside it — `document` (the document's title) and `title` (the
chunk's own heading), both additive and `omitempty`, on the off-run
`POST /v1/_memory/search` and the in-band `Memory op=search` alike.

The in-band surface also gains **`chunk_id`**, which it never had. The two projections
are hand-maintained copies, so an agent could be shown a document body it had no way
to fetch, while an operator on the HTTP endpoint got the id — a five-release drift
that only became visible once both were touched at once.

Labels resolve in ONE batched query per search, and are **best-effort by
construction**: they live in SQL Memory, a different plane from the bodies, so a
deployment without it, an unkeyable scope, a store fault, or a chunk deleted
mid-search each cost the label and never the result. The resolver owns the
Memory→SQL Memory scope mapping so neither caller restates it — the two planes key the
*tenant* scope differently (`""` against the tenant itself), and a restated rule is
how that axis drifts. A drift test compares the mapping against
`Document.resolveScope` directly, because a divergence there resolves against an empty
schema and the labels simply stop appearing, which is indistinguishable from "no
labels available".

The Memory console names a hit `document › heading` instead of printing a 32-hex id,
keeping the id on the tooltip. A document's root chunk carries the document's own
title, so the two are collapsed rather than rendered as `Verified writes › Verified
writes`.

One assertion in this change was **found vacuous and replaced**, which is worth
recording because the failure mode is not the usual one. To prove the resolver
deduplicates chunk ids, the test asked for each id twice and checked the label count
— and it passed with the dedup pass deleted, because `WHERE id IN (a,a,b,b)` returns
one row per DISTINCT id regardless: SQL `IN` has set semantics, so the assertion could
not see the difference. Nothing about the representation was misread; the layer below
already provided the property. The replacement pins a consequence only this code
controls — ids are deduplicated BEFORE the lookup is capped, so a page of hits from
one document cannot crowd out a different chunk's label.

### reembed embeds in batches, not one call per row (#1057)

A real embedder migration measured **~12 rows/minute**: 3,633 document-chunk rows
moving from a 768d model to a 1024d one, about five hours, because the loop called
`Embed` once per row and every row paid a fresh HTTP round trip and prefill setup
against a local Ollama. The `Embedder` interface has always been batch-shaped (N texts
in, N vectors out, chunked again by the driver's own batch size), so the per-row call
was leaving that on the floor.

Rows now reach the embedder in batches of 64 — two orders of magnitude fewer round
trips — while the STORE WRITE stays per row. That is what keeps the operation
resumable: a client timeout or a cancelled context mid-sweep costs only the current
batch, and the next call picks up what is left. It is also how an operator paginates a
scope too large for one request, so it is preserved deliberately. Not "all of them" in
one call, either: a single `Embed` over a whole 1000-row page would make one failure
cost the page, and document bodies can be large.

**A batch error falls back to per row.** One unembeddable row must not cost its
batch-mates: before batching, a bad row was counted and skipped while the rest
migrated, and that accounting is what an operator reads to decide whether a sweep is
done. Retrying singly restores it exactly — and as a side effect a transient one-shot
fault now recovers instead of stranding a row. The store write moved into one shared
helper so the batch path and its fallback cannot drift in what they record, and the
recorded dimension stays the OBSERVED width of the returned vector rather than the
embedder's advertised one (a driver that cannot know its own dimension answers 0, and
a row written with 0 makes every later search in that scope report a spurious
mismatch).

The existing partial-failure test had to be **reworked, not extended**: it asserted
the handler's CALL PATTERN rather than a guarantee. Its stub failed "the next call",
which under batching describes exactly the transient fault the fallback now retries,
so it reported 2 reembedded where it wanted 1 and 1. The stub now models a
permanently bad ROW and the test asserts the contract — an unembeddable row is
reported in `failed` and `failed_keys`, never silently dropped.

**Measured on the deployment that prompted it**: the migration that had been running
at ~12 rows/minute completed a 3,650-row scope, which now reports a single embedder at
a single width.

### Adapters

`@loomcycle/client` **1.58.0** carries the two new optional fields on
`MemorySearchEntry`. `@loomcycle/memory-view` **0.4.0** renders them. The Python
adapter is republished at **1.58.0** to keep the install-name version aligned with the
runtime; it has no changes this release.

## What's in v1.57.1

**An embedder migration could not reach the tenant holding the data.** Patch — one
server-side fix; no wire, schema or adapter change.

### reembed ignored ?tenant=, and reported a truthful-looking zero

`POST /v1/_memory/reembed` took the tenant from the caller's principal ALONE. An
operator migrating another tenant's scope after an embedder change therefore swept their
OWN partition, found nothing, and got `rows_total: 0` — indistinguishable from "already
migrated".

Observed on a live deployment mid-migration from `embeddinggemma` (768d) to `bge-m3`
(1024d): a legacy admin token carries no tenant, so every call resolved to the default
partition and reported zero, while `embed_stats` for the real tenant showed **3,633 rows
still on the old embedder against 17 on the new**. The console's own reembed plan showed
the same zero for the same reason.

What makes that worse than a wrong number: once ANY row of the new dimension exists, the
search-time dimension pre-check (which samples one row) stops firing. Semantic search
then returns only the post-switch slice, with plausible scores, instead of failing
loudly — so the operator has no signal that 99.5% of the index has gone unreachable.

`principalTenantScope` now resolves the tenant, so `?tenant=` reaches both the read and
the write-back, and an authenticated admin who names none is REFUSED rather than
defaulted — a reembed writes, and spends one embedder call per row, so sweeping an
unnamed partition is not merely a misleading count.

**The refusal is narrower than its siblings, deliberately.** `principalTenantScope`
reports all=true for a request with no principal, which is every request on an open-mode
deployment — and an open-mode install has no tenants, so demanding one there would break
a route that has worked since v0.9.0 to buy no safety. It fires only for an authenticated
admin; all five pre-existing reembed tests pass unchanged.

This was the third variant of one defect. `embed_stats` was already fixed to honour
`?tenant=`, and `backfill_embeddings`, `purge_stale_embeddings`, erasure, directory and
orphan-repair all guard the unnamed-tenant case. reembed was the one sibling with
neither.

**Why it survived:** the test double was blind to the tenant in three places —
`vectorAdminStore` dropped the tenant argument on three embed methods and keyed rows on
`scope|scope_id` alone, its list method joined the k/v row with a hardcoded empty tenant,
and its stats method prefixed without one. A double that ignores the dimension under test
cannot fail a bug in it. All three now carry the tenant through a shared `embedKey`.

### Release mechanics

A `vX.Y.Z` patch tag builds only the browser sidecar, and this fix is in the runtime
binary — so this release was published with `force_full=true` on the release workflow to
produce binaries and images. goreleaser's `mode: keep-existing` preserves the
annotated-tag notes the lean job pre-created.

## What's in v1.57.0

**Two defects that reached an operator, and a signal for the failure neither of them
could report.** Minor rather than a patch for two reasons: the change feed gains a new
wire field, and a `vX.Y.Z` patch tag builds only the browser sidecar — the fix that
matters here is embedded in the runtime binary, so it needs a full build to ship at all.

### The console's Backfill and Purge buttons threw (#1052)

Pressing either in the Memory console produced `e.backfillEmbeddings is not a function`.
Both methods landed in `@loomcycle/client` at 1.55.0; `web/package.json` depended on
`^1.49.0`.

**Why nothing caught it** is the part worth knowing. `@loomcycle/memory-view` compiles
from SOURCE through a Vite alias, and `web/vite.config.ts` lists `@loomcycle/client` in
`resolve.dedupe` — added, per its own comment, "for good measure — a duplicate is
wasteful even if not fatal". Dedupe makes web's copy the one that lands in the BUNDLE,
which turned a wasteful duplicate into a version DOWNGRADE. Both plausible guards are
structurally blind to it: `tsc --noEmit` (which web's build does run) resolves the
client from the memory-view source file — the package's own, correct copy — and
typechecks clean against a version the bundle will not contain; `vite build` strips
types and checks nothing. Green in CI and in `make build-ui`, broken only in a browser
at the moment a button was pressed.

`reembed` kept working throughout, because `reembedMemory` has existed since 1.49.0 —
which is why three buttons failed in two ways and looked like three unrelated bugs.

Fixed by bumping web to `^1.56.0`, verified by asserting the built bundle contains the
methods rather than by inference. A new **`TestWebDedupedDeps_SatisfySourcePackagePeers`**
makes it non-recurring: for every package web consumes from source, a peer dependency
that is also in vite's dedupe list must be satisfied by web's own range. It is a Go test
because `go test ./...` is the gate everyone runs and it needs no `node_modules`, and it
parses the dedupe list from the real `vite.config.ts` so removing a name there relaxes
the test honestly.

Also fixed alongside: `purge_stale_embeddings` advertised `agent, user, tenant` in its
validator and then demanded `scope_id` unconditionally, refusing the tenant scope its
siblings accept (the tenant keyspace is one partition with an empty store scope_id).
**Latent** — the client never had the method, so that request never reached the server.

### Non-Latin facts collided on one memory key (#1047, #1053)

`rawWords` tokenises on `/[^a-z0-9]+/`, so a fact in Cyrillic, Greek, Hebrew or any CJK
script reduced to no words and `slug()` fell back to the constant `"unnamed"`. `factKey`
is the consolidation pass's idempotency mechanism and the write path upserts on it, so
the SECOND such fact silently overwrote the first: a scope's entire non-Latin population
collapsed onto ONE row. Confirmed against the real engine — two different Ukrainian facts
both keyed `memory/fact/unnamed`. The entity-tier mirror used the same string as its
`natural_key` and collided identically.

An empty slug now appends a fingerprint of the text: a pure function of it (or a re-run
of the pass would duplicate rather than converge), two 32-bit hashes rather than one (a
single space collides at roughly a percent by ten thousand facts, and a collision is the
failure being fixed), shift-and-add only (the engine is ES5.1-era: no `Math.imul`, and a
32-bit multiply loses precision past 2^53), and base36 so the key stays inside
`[a-z0-9-]` — it is interpolated into SQL by the lookup path.

**It does not widen the tokeniser.** A non-Latin fact still derives no source span, so it
stores unverified-and-visible rather than verified. That change redefines what a "word"
is and needs the `merge_min_subject_overlap` floor re-measured against its labelled
pairs; it is deliberately separate. This stops the data loss.

⚠️ **It cannot recover what was already overwritten.** On a store that held several
non-Latin facts, only the last survives, and this release prevents further loss rather
than restoring the earlier ones.

### The change feed says what the embedder is doing (#1051)

An embedder outage is invisible at every call site, by design: a failed embedding on a
content write is not fatal — the body is stored and the embedding skipped with a log line
— because losing an author's text to an unreachable embedder is worse than losing its
searchability. The consequence is that writes succeed, change frames keep arriving, and
search quietly stops finding anything written during the outage. A reader watching the
Activity tab to answer "is the memory pipeline healthy" saw a busy feed and concluded yes.

The opening `feed` frame now carries an additive `embedder` block: `state`
(`absent` | `untried` | `ok` | `failing`), provider, model, calls, failures,
`last_failure_kind`, and the last ok/failure stamps. Not a probe — a probe would cost a
model call per reader and describe one instant; a new `providers.ObservedEmbedder`
decorator records what the traffic that already went through actually did, so the answer
cannot disagree with reality (the same argument as `cdc.Store.CapturesChanges()`).

Four decisions with a plausible wrong answer, each pinned by a test: `untried` is its own
state, because a configured-but-never-called embedder must not report `ok` — that is the
state a freshly booted deployment is in; the failure COUNT is reported, not a boolean,
because it is roughly how many rows need `backfill_embeddings`; no error TEXT escapes,
since a transport failure reads `dial tcp 192.168.0.77:11434: …`, a map of the operator's
network, so failures are a classified kind matched by `errors.Is`; and it is separate from
`enabled`, because capture being on and the embedder working are independent failures.

`@loomcycle/memory-view` **0.3.1** surfaces it as one sentence for a failing or absent
embedder, and stays silent for `untried`, `ok`, and a runtime too old to send the field —
warning about the normal boot state would train an operator to ignore the only line that
matters when it fires.

### Adapters

`@loomcycle/client` and `loomcycle` (PyPI) go to **1.57.0** as version-aligned lockstep
releases; no client-surface change (the `embedder` block rides an SSE frame the TS client
already forwards, and the fixes are runtime-side). `@loomcycle/memory-view` **0.3.1**
publishes on its own tag.

### Upgrading

Nothing to migrate, and the two operator-visible notes are above: the console fix is
embedded in the binary so it arrives only with this build, and the keyspace fix prevents
further collisions without recovering earlier ones.

## What's in v1.56.1

**A tool-call regression on qwen-via-Ollama, and the v1.56.0 bundle change that caused
it.** Patch — one runtime fix and one config revert; no wire, schema, or adapter change.

### qwen on Ollama stopped calling tools

A `chat/medium` run on a local qwen3.6 model wanted to search but emitted its call as
TEXT, copying the framing of loomcycle's own injected `{{tool:Context.*}}` reference
blocks — `<tool_result> {"type":"function","function":{"name":"WebSearch", …}} </tool_result>`.
Ollama's extractor expects `<tool_call>`, so it recovered nothing, and the existing
text-recovery parser only understood a flat `{name, arguments}` object. The wrapped,
OpenAI-nested shape fell through, and the run ended with the un-executed block as its
"answer" — no search ran.

- The Ollama driver's `tryParseToolCallsFromText` now peels one wrapper tag
  (`<tool_call>` / `<tool_result>` / `<function_call>`, hyphen or underscore, with or
  without attributes) and accepts the OpenAI-nested `{"function":{name,arguments}}`
  envelope, with arguments as an object or a JSON-encoded string. Same strict,
  tools-gated contract — prose and non-tool JSON still recover nothing.

### inject_tool_guide is no longer a bundle default

Making `inject_tool_guide` a bundle default in v1.56.0 was premature: its target is
small local models, and that is exactly where the three injected `<tool-result>`-framed
blocks both bloat the prompt and get copied by the model as its tool-call format. The
flag is removed from every bundled agent; the mechanism — `Context op=guide`, the
`HintedTool` hints, the injectable refs, and the per-agent flag — stays intact for
explicit opt-in on a strong-model agent.

## What's in v1.56.0

**Agents that know how to call their tools, a live view of the memory pipeline, and
three defects the first live verified-writes run turned up.** Minor — two feature
lines and three fixes, two of which change numbers an operator may be watching.

### Agents start with runtime knowledge instead of blind

The loop already sends every tool's full JSON schema on each request, but small and
local models under-attend to that array: they guess which op to call and which fields
are required, then learn from refusals. The system-prompt injector could restate tool
NAMES and nothing about how to use them.

- **`Context op=guide`** returns, for this run's resolved tools, `{name,
  side_effect_class, ops[], required[], hint}`. The ops and required fields are parsed
  from each tool's OWN input schema, so the digest cannot drift from the schema the
  model is being sent; the hint comes from a new optional `tools.HintedTool`,
  implemented on the builtins that produce the most call errors — Memory, Document,
  Path, Channel, Agent, Skill.
- **`{{tool:Context.guide}}` and `{{tool:Context.capabilities}}`** are now injectable
  prompt references (both pure reads, resolvable at assembly time), sorted for
  byte-stable prompt caching.
- **A per-agent `inject_tool_guide`** appends both refs automatically, so an operator
  need not hand-place placeholders. **Default OFF** — every existing custom agent stays
  byte-identical until it opts in, and a flag-OFF agent takes the unchanged fast path.
  Round-tripped through the AgentDef create/fork overlay, the read adapter, the
  MD-frontmatter loader and `content_sha256`, mirroring `memory_protocol`.
- **The bundled LLM agents opt in**: `chat/*`, `doc/manager`, the agent-teams and
  team-examples agents, and `memory/ontologist`. Deliberately not flagged: the doc/team
  sub-agents are SKILLS injected into the flagged agents (the field has no meaning on a
  skill), the `code-js` agents run no prompt, and `memory/extractor` / `memory/judge`
  hold no tools so the guide would render nothing.

### The memory console can watch the pipeline work

RFC CF's last phase. A consolidation pass takes minutes and reports only at the end, so
there was nothing to look at in between.

- **Both change feeds now open with a `feed` frame** stating whether writes are actually
  being captured, plus the cursor in force. This closes a real trap: with
  `LOOMCYCLE_MEMORY_CHANGES_ENABLED` unset nothing writes the change table, so the
  stream connects, keepalives flow, and no frame ever arrives — indistinguishable from a
  healthy feed over a quiet store. The answer comes from the STORE (a new
  `cdc.Store.CapturesChanges()`), because the CDC decorator is in the write path exactly
  when the feed is on and therefore cannot disagree with reality the way a second reading
  of an env var can. Additive: a subscriber that switches on the change type ignores an
  event name it does not know.
- **An Activity tab** in `@loomcycle/memory-view` 0.3.0 tails both families, showing
  coordinates and never values (the feed is value-free by design). It keeps three
  non-live states apart — this build cannot tail / the runtime says the feed is off /
  connected-and-quiet — because collapsing them into "no rows yet" makes two
  misconfigurations look like "the pass is doing nothing".

### Three fixes, from running verified writes on real data for the first time

The verified-writes line shipped in v1.54.0 and had never been enabled anywhere. Turning
it on against real chats, with a local extractor and judge, worked — and exposed three
defects that only appear on real data.

- **`verification_stats` counted entity IDENTITY nodes as facts.** The pass mirrors each
  fact as two chunks — the claim, and an identity node for the subject it is about — and
  both carry entity metadata. An identity node can never carry a span (a subject is a
  name), so every new subject added a permanently unverifiable row and the reported share
  FELL as the store got richer. Measured on a live store: `0.579` reported where `0.846`
  was true, and 7 facts called impossible to verify where the answer was 1.
  **⚠️ Your numbers will move on upgrade** — the verified share rises and
  `unverifiable_no_span` falls, with no data change behind it. `list_facts` gains an
  opt-in **`claims_only`**; the default listing stays wide because document federation
  reconciles identity nodes too. The obvious fix was wrong and measurement caught it:
  filtering `type = 'fact'` drops operator-`remember`ed facts, one of which landed as
  type `object` while carrying a span.
- **Span derivation attached SEARCH PATTERNS as evidence.** Two consecutive passes each
  withheld a TRUE fact whose recorded span was a tool-call line or a regex — a local chat
  model had written its tool calls as prose, and Dice overlap rewards a short candidate
  whose every token hits, so a 6-token pattern beat a 30-token sentence that stated the
  fact outright. The judge was right to refuse both; the evidence was mis-attached
  upstream. `splitSentences` now drops a candidate with no assertion in it — one that is
  only tags, or dense enough in punctuation to be code. Such a fact gets NO span
  (unverified and visible) rather than a false one. Does not re-derive spans already
  stored.
- **`loomcycle validate` exited 2 on every bundled agent preset.** It applied the pin-path
  rule to agents that name a `tier:`, so it named agents the running server resolves
  fine — and because it returns at the first agent, MCP servers and everything after went
  unchecked. `agents list --json` bailed mid-array, leaving unparseable output. The tier
  is now checked first, mirroring the runtime; an agent with neither a pin nor a tier
  still fails, which the resolver also refuses.

### Adapters

- **`@loomcycle/client` 1.56.0** — carries `claims_only` on the Document tool input.
- **`loomcycle` (PyPI) 1.56.0** — no functional change; realigned so the python tag
  publishes (PyPI was left at 1.54.0 when v1.55.x shipped without a python tag).
- **`@loomcycle/memory-view` 0.2.0 → 0.3.0**, published on its own tags: the facts and
  verdict surface, the coverage strip, `remember`, the two embedding-maintenance ops, the
  claims-only fact list, and the Activity tab.

### Upgrading

Nothing to migrate. The change feed stays opt-in (`LOOMCYCLE_MEMORY_CHANGES_ENABLED=1`),
`inject_tool_guide` is off unless an agent sets it, and `verify_writes` is still off by
default. The one visible difference on an existing store is the corrected coverage
arithmetic described above.

**Known issue:** facts whose text is in a non-Latin script all slug to one memory key and
overwrite each other ([#1047](https://github.com/denn-gubsky/loomcycle/issues/1047)) —
pre-existing since the deterministic-key consolidator, unaffected for Latin-script
deployments.

## What's in v1.55.1

**An operator may vouch for a fact that has no span.** Patch — one reported bug, its root
cause, and two pieces of copy that were saying something untrue.

- **The bug.** Opening a fact stored before spans existed, writing "User confirmed", and
  being refused. Both controls failed there — only `unclear` was allowed without a span —
  so the console offered two buttons the server would never accept.
- **The rule's own justification had expired.** `judge_fact` refused because a verdict
  without evidence "would be indistinguishable from one that was checked". That stopped
  being true when `judged_by` landed in v1.54.0: the store now records whether a machine
  or a human reached a verdict, so an operator putting their name to a fact is no longer
  mistakable for an entailment check. **An operator** may now record `supported` or
  `unsupported` on a span-less fact — no span is invented, it still counts as span-less in
  coverage, and the reason plus `judged_by` carry whose word it is. **An agent still
  cannot**: a run affirming what it cannot check is exactly the claim a span exists to
  substantiate. **`mistyped` is excluded for anyone** — it says the span carries the claim
  but the filing is wrong, so with no span there is nothing for it to be about.
- **Copy.** The console now distinguishes CHECKING from VOUCHING at the moment your name
  goes on a verdict; and the coverage strip no longer describes span-less facts as ones
  that "can never be verified by anyone" — they cannot be checked against a source, but a
  person can still vouch.

Built FULL via a `force_full` dispatch despite the patch tag: the fix is in the Go runtime,
and the lean patch tier builds only the `loomcycle-browser` image, which would have left
the change unable to reach a deployment. The Web-UI half lives in
`@loomcycle/memory-view`, which publishes on its own `memory-view-v*` tag.

## What's in v1.55.0

**One memory console, and an erasure you cannot perform unrecorded.** Minor — the memory
surfaces built over the previous release are consolidated into a single console, an
operator can write a fact down themselves, retention is finally readable, and subject
erasure grows a durable record of who asked for it. Plus Web-UI authoring for the two
peer-federation substrates that shipped headless in v1.54.0.

### RFC CF — the memory console

- **One console, not two.** v1.54.0 added a facts panel and a search panel to the Web UI
  beside `/memory` — which is a thin wrapper around `@loomcycle/memory-view`, a published
  package that already shipped `entries` | `facts` | `search` tabs. The duplicates are
  removed and the genuinely new capability folded into the package: **spans, verdicts,
  `include_refuted`, and the two controls that let a person overrule the judge**, plus a
  coverage strip over the fact list. The line drawn: the package owns views over one
  scope's data, the app owns the operator plane — so starting a consolidation pass is a
  host-supplied callback rather than a URL the package knows.
- **The safety valve exists now.** Verified writes shipped with "a wrong verdict is always
  recoverable by re-judging" as its argument for withholding rather than deleting, and
  that recovery was reachable only by API. An operator can now see the claim, the span it
  was drawn from, and the verdict — and overrule it in either direction.
- **`judged_by`** records WHO reached a verdict, server-stamped from the call's own
  context exactly as `origin` is. There is no wire field to set it: an agent able to
  record "an operator decided this" would be one injected instruction away from laundering
  a machine's verdict into a human's. An agent's verdict carries the agent's NAME; a
  verdict predating the column reads as unknown rather than as either party.
- **`Document op=remember`** stores a statement a person supplied as a fact that **cites
  itself** — the text becomes both the claim and its source span, filed `evidential`. An
  operator's instruction is a source, not a claim; storing it self-citing makes
  operator-authored memory the best-evidenced kind rather than the worst. Additive only:
  there is no "forget", because an instruction that deletes on a fuzzy match is how data
  disappears quietly.
- **Retention is readable.** `GET /v1/_retention` gets a Settings surface: what the sweeper
  is configured to remove, per family, with the sweeper's own on/off state stated first.
  A `purgeable` count is never rendered alone — it is computed regardless of mode, so
  beside an `off` family a bare number reads as a countdown to a deletion that never
  happens.
- **Settings tabs get the three-tier role class** the left nav already had. The binary
  `admin` boolean could not express a tenant operator, so a delegated user reaching
  `/settings` directly was shown five tabs whose every call 403s.

### Subject erasure — audited, or refused

- **An erasure now writes its own durable record, and refuses without one.** The audit sink
  says recording "must never block the caller's primary operation"; erasure is the
  deliberate exception, because it is the one operation nothing can undo. A deployment
  that cannot say who erased which subject does not get to perform the deletion — refusing
  is recoverable, an unrecorded erasure is not. With no `LOOMCYCLE_AUDIT_LOG_PATH`,
  erasure is disabled and boot says so.
- **Two records, ordered.** `erase_intent` is written BEFORE any deletion and its failure
  refuses the operation; `erase_result` follows with the planes deleted and retained. A
  crash between them still leaves evidence that an erasure was attempted, by whom, against
  which subject. Dry runs write nothing.
- **The console: report before execute, always.** The erase control does not lift until a
  report has been shown for the SAME subject — otherwise an operator could read what would
  go for one person and confirm the deletion of another, which no confirmation dialog
  catches. Confirmation is the subject id typed back and compared exactly: a modal with a
  red button is dismissed by reflex, and the failure mode here is erasing the WRONG
  subject rather than erasing accidentally. The three tiers are never summed — they are
  degrees of REACH, so each is labelled by what it does ("will be deleted" / "held, but
  NOT deleted" / "cannot be reached") — and a residue of zero across zero sessions reads
  as UNKNOWN rather than none, since with no sessions examined there was nothing to trace
  derived data from.

### Web UI — the peer-federation substrates

- **Remote memory backends** (RFC CD Part B) are authorable from Integrations: a kind
  selector reveals the peer connection — `base_url`, `api_key_env` (an env-var NAME, never
  a secret) and an optional `api_version`.
- **Document Sources** (RFC CE) join them as a fifth Integrations family, so a peer
  `DocumentSourceDef` can be authored, forked and retired from the Web UI rather than only
  from yaml, the substrate API or MCP.

### Adapters

- `@loomcycle/client` gains **`backfillEmbeddings()`** and **`purgeStaleEmbeddings()`** —
  the two embedding-maintenance ops that had no client method. `@loomcycle/memory-view`
  will surface them once it can depend on this release.

## What's in v1.54.0

**External data access, document federation, and verified writes.** Minor — three feature lines land together: any app reaches loomcycle's memory + documents over a documented HTTP contract or a peer loomcycle (RFC CD), a document is replicated to and reconciled with a peer instance in both directions (RFC CE), and a fact records the source it came from and is checked against it before it is trusted (RFC CC). Plus the loomboard saved-views scaffold (RFC BT) and a memory/facts Web-UI surface.

### RFC CD — external data-access channels

- **OpenAPI 3.1 contract + Swagger UI.** A hand-authored `api/openapi.yaml` documents the whole data surface — the memory REST family + `POST /v1/_memory/search`, the `/v1/_document` and `/v1/_path` op-dispatch (modeled as a `oneOf` on `op`), and the asset GET — served at `GET /v1/openapi.{yaml,json}` with a self-contained Swagger UI console at `/v1/docs` (vendored, no CDN). Any language gets a generated SDK; the contract is public, the data still bearer-gated. A drift test pins the spec's op enums to the tool op sets.
- **A peer as a memory backend (`kind: remote`).** A memory backend can now proxy an agent's memory to *another* loomcycle instance's `/v1/_memory/*` — the peer embeds server-side; get/set/list/delete/search/stats all round-trip. Auth is a credential-allowlisted `api_key_env` (never an infra secret, never `${...}`-interpolated), resolved at use time; the peer host is dialed through the SSRF guard; `fallback_on_error` degrades to local.
- **Change feed (CDC), pull + push.** An opt-in, value-free change feed (`LOOMCYCLE_MEMORY_CHANGES_ENABLED`) emits at the store write choke point so both in-run and external CRUD land in one stream. Consumers subscribe over SSE (`GET /v1/_memory/changes`, `/v1/_document/changes`) or register a config-declared `change_subscriptions:` HMAC-signed webhook with a persisted at-least-once cursor. Tenant-scoped, SSRF-allowlisted; the feed is operator observability (`substrate:tenant`, not member-accessible).
- **Python gRPC memory parity.** A generic `Memory` RPC + `client.memory()` gives the Python adapter the memory surface it lacked, riding the same op-dispatch shape as documents and paths.
- **Ops:** every published image is mirrored to `ghcr.io` so a Docker-Hub-rate-limited operator has a fallback.

### RFC CE — remote document backend + federation

- **Bind + reconcile a document with a peer.** Declare a peer under `document_sources:` (or author one at runtime — see the substrate below), bind a local document to a peer document with `Document op=set_remote`, and reconcile with `op=sync`. `direction:pull` (default) copies the peer's chunks in; `direction:push` writes this document's chunks up to the peer. Reconciliation keys on `natural_key` and carries each keyed chunk's **body, tags, hierarchy** (it lands under its keyed parent at its sibling position) and its **manual cross-reference links**; a diverged body is updated in place with the overwritten body kept in the *losing side's* chunk history (retire-not-delete); a chunk without a `natural_key` is excluded and counted.
- **`op=diff_remote`** — a read-only dry-run that classifies keyed chunks into `only_local` / `only_remote` / `diverged` / `retagged` / `reparented` / `same` (plus unkeyed + edge-drift counts) without touching either side, so you see exactly what a sync would change first.
- **`DocumentSourceDef` substrate** — a source can be authored, forked, and retired at runtime as a versioned, tenant-scoped substrate Def over every transport (HTTP / gRPC / MCP / TS / Python), a faithful mirror of `MemoryBackendDef`; `set_remote`/`sync` resolve a name tenant-substrate → static yaml → shared substrate.
- **Reviewed + hardened.** A whole-line adversarial code review surfaced eight findings, all fixed with fail-before regression tests: sync now converges on a refuted chunk (no create-churn), the `DocumentSourceDef` HTTP route is `substrate:tenant` in parity with gRPC/MCP, title/type/status edits propagate, `diff_remote`'s reparent prediction matches what a sync does, a retired source stops resolving, a tags-only sync no longer writes a duplicate-body revision, and the runtime + static source validators accept the same values.

### RFC CC — verified writes

- **A fact records where it came from and is checked against it.** A fact now stores the exact `source_quote` span it was derived from (RFC CC P1); the subject becomes structured data the ontology gates entity writes against (P2); a write-time judge checks each fact against its own quote and **withholds** what fails (a refuted fact is retained but hidden from `list_facts`/`graph_recall`, readable with `include_refuted`), rather than deleting it. `verbatim_answer` answers a lookup question with a stored fact quoted verbatim plus its span and no generated text; a backfill sweep judges facts that predate the feature; `judged_at` records *who* judged a fact in a column the caller cannot set.
- **Web UI.** A Memory tab surfaces verification coverage and lets an operator start a pass; a facts surface shows evidence, verdicts, and a way to overrule the judge; semantic search over memory is the front door to the facts panel.

### Adapters + packages

- **`@loomcycle/client` 1.54.0** (npm) — adds `documentSourceDef()` and the accumulated substrate/memory surface.
- **`loomcycle` 1.54.0** (PyPI) — adds `memory()` (gRPC) and `document_source_def()`.
- **RFC BT** — the `@loomcycle/loomboard` saved-views scaffold (P1) + the board-bound `TeamDef op=run` task-key tagging (P4); loomboard versions on its own `loomboard-v*` tags. The React `explorer`/`library`/`memory-view` packages version independently.

## What's in v1.53.2

**Tenant-member access: a non-isolated user reads and writes the tenant plane (RFC CB).** Patch (Go, auth only). A delegated per-user token that is not isolated is now a full tenant *member* over HTTP.

**The gap.** A non-isolated user token (a "member" — `runs:*`/`channel:*`, `access_mode "tenant"`) already had whole-tenant *data* access inside a run — loomcycle's rule is "whole-tenant data access is conferred by NOT being isolated." But the same token was 403'd at every HTTP `/v1/_*` tenant surface by the route scope-gate alone, so a member could not browse or author its tenant's shared Library, Documents, or Memory over HTTP, and consumers (loomboard) hid those views rather than render a wall of 403s.

**The change.** `authMiddleware` now admits a non-isolated principal on a *member-accessible* tenant route: `tenantMemberAccessible(method, path) && !auth.IsIsolated(p)`. The predicate opens any route that requires `substrate:tenant`, **minus** the operator-control carve-outs — user create / roster mutation / user-token minting, per-subject erasure, budget *writes* (`PUT`/`DELETE /v1/_limits`), and tool-use hooks — which stay operator-only. `substrate:admin` routes (cross-tenant enumeration, runtime admin, token minting) are excluded automatically, since they are not `substrate:tenant`.

**No new scope, no re-mint.** It keys on the existing isolated/non-isolated boundary — generalizing RFC BY's discovery tiering to the route gate — so no token changes. The isolation floor is untouched: an isolated `substrate:user` token still fails every tenant gate, `auth.IsIsolated` stays true for it, and `ConfineIsolatedScope` plus the run-start isolation stamps are unchanged. A member is still confined to its own tenant by each handler's `principalTenantScope`/`tenantFromCtx`.

**Scope.** The HTTP gate. gRPC/MCP substrate-plane parity is deferred (a member's runs already reach tenant data on those planes since they are not isolated). loomboard's `canTenant` relax — so a member renders the Library/Documents/Memory nav — ships in loomboard separately. No adapter or Web UI changes here.

## What's in v1.53.1

**The routing view stops blanking on an empty tier cascade.** Patch (Go + Web UI). The Settings → Routing page went blank on a real deployment, and the cause was a `null` where the UI expected a list.

**The bug.** `GET /v1/_routing` builds each tier's candidate cascade by appending to a nil slice, so a `user_tier × tier` that resolves to **zero candidates** — a perfectly valid config state, e.g. a `high` user-tier whose `high` tier has no available model — came back as JSON `"cascade": null`. The routing view's `TierCard` does `tier.cascade.length` on that; `null.length` throws, React unmounts, and the whole page is blank. Same class as the `/v1/_usage` nil→null crash in v1.11.1.

**The fix, at both ends.** `handleRouting` now initializes the `Tiers` and `Cascade` slices to non-nil empties, so an empty resolve serializes as `[]`; and `RoutingView` adds `?? []` guards on `user_tiers` / `tiers` / `cascade` so a single bad tier can never blank the page again. A regression test asserts an empty cascade decodes to a non-nil slice (verified failing on the old code).

**Built with `force_full` on purpose.** Like v1.52.1, this Web-UI-touching patch was cut with the full pipeline so it publishes the deployable `denngubsky/loomcycle` image with the fixed UI embedded — a lean browser-patch would ship only `loomcycle-browser`, which the standard deployment does not consume.

Adapter versions are unchanged — no adapter surface was touched.

## What's in v1.53.0

**Ontology curation: an agent can suggest entity types, and only an operator can accept one.** Minor — a new inert entity status, two operator actions, one narrow authoring op, a bundled curator agent, and a refreshed MCP tool surface. No schema change; one behaviour change for agents that were writing the ontology document directly (below).

**The problem.** The ontology had a single gate — the root chunk's `draft`/`confirmed` status — so anything appended to a confirmed document was live on the next run. There was nowhere to put a suggestion that is real but not in force, which meant an agent could not *suggest* a type, only add one. Separately, overriding a standard type meant reading its field names off the panel and retyping them by hand.

**A proposal is the entity in its final place, switched off.** `chunks.status` was already a column the ontology reader ignored, so `proposed` and `rejected` are now reserved as inert: such a chunk is not an entity, not rendered into any prompt, and not expanded in retrieval — but it *is* reported to the operator, in its position in the tree, with its fields and its body. Accepting clears the status in place, so an accepted subclass lands under the parent it was filed under and inherits from it. Nothing is copied out of a staging area.

**Only those two words are inert, and that matters more than the feature.** Every other status — including one the build has never heard of — leaves the entity in force. The shorter inverse rule ("anything not blank or confirmed is inert") would silently drop types from any document where an operator had used the status field for their own purposes, which is exactly the failure the chunk-tree reader was written to remove. Children of an inert chunk re-parent to the nearest in-force ancestor for the same reason: rejecting a parent must never switch off a live type beneath it. A **rejection is kept** as a tombstone, so an automated curator can see what was already turned down instead of re-proposing it.

**Adopt a standard type.** One action copies a standard type into the document with its fields, so it can be extended or subclassed without transcription. The fields come from the seed **server-side** — a client-supplied list could disagree with the type it claims to copy, and the document would look like an override of `event` while declaring something else. It records provenance (what was copied, from where, at which build) in the chunk's structured fields, never its body: adoption **freezes** your copy, since an override replaces the standard term wholesale, and without provenance that is undiscoverable later.

**An agent may propose, never decide.** On the tenant ontology document alone, a tool call from a run may only add a chunk whose status is `proposed`. Update, delete, move, reorder, supersede, `import_md`, `import_canvas`, `delete_document` and `set_path` are all refused there — the last two reach the whole ontology, `set_path` by re-homing `/memory/ontology` so the reader resolves somewhere else. The operator/agent distinction is a server-stamped context marker with **no wire form**: the off-run path also carries a synthetic agent id, and `POST /v1/runs` accepts a caller-supplied `agent_id`, so keying on that would have let a run claim to be the operator.

**`propose_entity`** is the front door: it resolves the ontology itself, takes the parent **by name** (the names the agent was given in its prompt — it cannot know a chunk id), stamps the inert status so the contract cannot be spelled wrong, bounds the evidence body, and refuses a name already in force, already proposed, or already rejected. It needs **no tenant grant**, deliberately: a suggestion cannot change what any run is told, so requiring the authority live authoring needs would mean only an agent that could already author live types could offer one.

**The bundled `memory/ontologist`** does a pass over one user's stored facts and files suggestions with evidence — counts and example titles — reachable from Settings → Ontology by a link that *stages* the run in the terminal rather than starting one from a settings click. On demand, not scheduled: a curator filing proposals nobody reads is clutter with a cron job. It is one agent definition; no new runtime primitives. Measured on a local model it is useful but not reliable, and its value depends on facts carrying types at all — measure your extraction model's type-emission rate before scheduling a pass.

**The MCP tool surface was refreshed, and had two problems.** The Document description was 13 ops behind the tool (`query_documents`, `list_facts`, the tag ops, the history ops, `backlinks`, `related`, `unlinked_mentions`, the canvas ops) — for an MCP client the description *is* the documentation, so an unlisted op is one no model will call. And 18 internal design-document citations had accumulated, 13 inside `Description` strings that go in front of every client's model. Both are now enforced by tests. The Claude Code plugin's reference is updated to match (plugin PR #26); it ships no tool schemas of its own, since the thin client proxies this `tools/list`.

**⚠️ Behaviour change.** An agent that was writing the ontology document directly will now be refused everything except filing a proposal. An MCP session is an agent surface, so an operator driving the thin client can no longer flip the ontology's confirm status either — the Web UI and `POST /v1/_ontology` remain the operator path.

Adapter versions are unchanged: `@loomcycle/client` 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/memory-view` 0.1.0. `@loomcycle/explorer` 0.7.0 publishes on its own `explorer-v0.7.0` tag, carrying the `+ child` nesting fix from v1.52.1 to external consumers.

## What's in v1.52.1

**Patch: the ontology was not editable through the UI, for two independent reasons.** v1.52.0 shipped RFC BZ's reader, inheritance, retrieval expansion and panel tree — and no working path for the operator who owns the taxonomy to author one. Both bugs were reported from a real deployment.

**The document viewer could not nest anything, anywhere.** `+ text` inserts a SIBLING (`after_id`), which is right for prose flow and wrong for structure — so a hierarchy could only ever be created by `import_md` or by an agent. Selecting a type and adding a sub-item put it BESIDE that type at the top level. A new **`+ child`** action passes `parent_id`, appending the new chunk under the selection and opening the editor on it so it can be named immediately (a type's name is its heading). `+ text` keeps its sibling behaviour; the tooltips now say which is which.

**And the Settings panel's "Edit ontology →" link never worked.** It pointed at `/documents/<id>` with no scope. The ontology lives at *tenant* scope and the viewer folds an absent scope to `user`, so the link opened the right document id in the wrong store, the read 422'd, and the operator got "No chunks" and no create buttons at all — indistinguishable from "editing is not possible here", and most of why the UI read as unusable. The URL is now built by a tested helper rather than an inline template literal, because a dropped query param is exactly what nobody notices. This was the only `/documents/` deep link in the codebase, so no other surface carried the same defect.

The panel also now names the steps — select a type, then `+ child` — which is not something anyone can guess.

**Release note for operators:** this is a Web-UI fix, and the Web UI is embedded in the binary and the `denngubsky/loomcycle` image. The patch build tier publishes only `loomcycle-browser`, so this tag was released with the `force_full` dispatch to produce the full artifact set (binaries, Homebrew, and every image variant) — the escape hatch the release workflow documents for exactly this case. The RFC BR sandbox sidecar images are unchanged and were not rebuilt.

Nothing else changed: no Go code, no schema, no wire surface. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0 and `@loomcycle/memory-view` 0.1.0. The `+ child` fix reaches the Web UI through the package source (Vite alias); publishing it to external `@loomcycle/explorer` consumers needs an `explorer-v*` bump and tag.

## What's in v1.52.0

**RFC BZ: the tenant ontology gets classes and subclasses — one chunk is one entity, and a child chunk is a subclass.** Minor — no schema change and no wire-breaking change, but retrieval returns strictly more for a hierarchical ontology, and a bug fix makes types live that were silently inert. Read the upgrade note below before confirming an ontology you had already nested.

**The bug it fixes, and why it needed a new reader.** An operator who organised their ontology into a hierarchy — the natural move in a document UI — lost every nested type. `ParseOntologyMarkdown` matched only `"## "`, so a `### incident` was recognised as neither a term nor a title and was dropped: no error, no warning, nothing in the Settings panel. Their document read correctly and their ontology did not match it. "Also match `###`" cannot fix it, because the ontology was read from `export_md`, which renders chunk depth as `strings.Repeat("#", level)` — after flattening, a subclass's title and a heading inside a body are the same bytes, so the information needed to tell a nested TYPE from a nested COMMENT is destroyed before the parser sees it. The reader now walks the chunk tree, where `parent_id` *is* the hierarchy: the chunk's title names the entity, backticked bullets are its fields, and a child chunk is a subclass. Depth is capped at four levels and a deeper type is flattened onto the cap rather than dropped — silently discarding an entity is the bug being fixed, and a cap that did it one level down would be the same failure.

**A subclass inherits its parent's fields.** `incident` under `event` carries `occurred_at` without restating it, transitively; a child redeclaring a field wins. An inherited field cannot be removed, which is correct rather than a limitation — a subclass lacking its parent's field is not a subclass, and someone who wants a type without those fields wants a sibling. Removal lives on the other axis, where a tenant term replaces a same-named seed term wholesale, so resolution runs *after* layering: a subclass of a type the tenant redefined inherits the tenant's fields, not the seed's. To subclass a *standard* type, declare it as your own root (which overrides the seed by name) and nest beneath your copy.

**The prompt renders a tree, and a flat ontology renders byte-identically.** Extraction now sees indentation plus one instruction — use the most specific type that fits — because a model handed a ladder with nothing telling it to climb sits at the top and the subclasses go unused. The tree form engages **only** when a hierarchy is actually present, and a dangling parent does not count: this string is in the system prompt of every extracting agent, so a whitespace or ordering change would invalidate provider prompt caches and shift extraction results for every deployment that never nests. That guarantee is asserted by test, not assumed.

**Retrieval expands subtypes — the payoff.** `list_facts` and `query_chunks` now match a type *and its subclasses*, transitively and downward only: `event` finds `incident` and `outage`, while `outage` still finds only outages. Expansion happens at query time and storage keeps the concrete type only, so re-parenting a type takes effect immediately and retroactively; materialising ancestors into each row would mean a correction silently invalidates every fact written before it. It applies **only when the ontology is confirmed** — a draft is inert everywhere else, and retrieval must not be the one surface where an unconfirmed edit changes answers — and the response reports `type_expanded_to` when the filter widened, because a wider answer with no explanation reads as a bug. `graph_recall` is not included: its seeds come from title text or explicit ids, so it has no type filter to expand.

**The Settings panel shows the taxonomy it now parses.** Both columns render as trees with field counts, declared fields plainly and inherited ones dimmed, plus three signals that were previously invisible: the depth cap says when it flattened a document, a leaf declaring no fields of its own is flagged (almost always a section chunk that silently became a subclass), and an awkward type name gets an advisory. Two columns are kept rather than merged into one tree — the gap between "this deployment defines" and "in force now" is the only visible evidence that a draft is inert.

**`preference` and `fact` are pinned as roots.** They may be *subclassed* — `dietary-preference` under `preference` is fine — but they may not be given a parent: that inverts the memory tier, and with subtype expansion a query for that parent would sweep in every preference the user ever expressed. Enforced by clearing the parent rather than rejecting the document, and reported in the panel. **Type names are warned about, never rewritten:** a name is part of a stored fact's key, so normalising `Notes on naming` after facts exist under that spelling would split the type in half.

**The extraction eval can now measure hierarchy use, and the measurement overturned the assumption behind it.** A new `specificity` ability, a hard type assertion, and a separate `--corpus hierarchy` fixture (kept separate so the shipped corpus digest — and its recorded baselines — stay valid). The RFC expected *over-specification*: given a ladder, a model reaching for `incident` where `event` was right. Across four runs of `ollama-local qwen3.6` it never once climbed past the evidence; what it does is decline to type at all on the subtype cases and fall back to the standard roots. Under-engagement, not over-reach. No baseline entry was recorded on purpose — four cases swinging 0.25–1.00 cannot support a 0.15 tolerance, and a flapping gate teaches everyone to pass `--no-gate`. Separately verified: the seed-only extractor prompt still digests to a recorded baseline's value, so nothing in this release invalidated the committed numbers.

**⚠️ Upgrade note.** The fix is a migration event. Types you nested and saw no effect from were inert; after this release they are live subclasses and they steer extraction, and a type filter returns strictly more than it did. Re-read your ontology in Settings → Ontology before confirming it — the panel now shows the tree it actually parses, so the change is visible in one screen rather than discovered through extraction results.

`/v1/_ontology` gains `parent`, `inherited` and `name_issue` per term and reports `notes` (a list) in place of the single-string `note` introduced earlier in this same unreleased line, so no released consumer saw the old shape. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0 and `@loomcycle/explorer` 0.6.0 — none was touched.

## What's in v1.51.0

**RFC BY: a user token can now read its own chats and discover the agents it may run.** Minor — one new endpoint, one additive `@loomcycle/client` method, and a widened-then-reconfined gate. No schema change, no wire-breaking change. Completes RFC BX's "user access = own + bundled + tenant-if-enabled" model on the read side.

**The gap it closes.** RFC BX gave a `substrate:user` token the run plane but no read surfaces. A delegated user could start and read its own runs and nothing else — it could not see its own past chats, and it had no way to find a runnable agent without being told the name out of band. Both surfaces were gated at `substrate:tenant`, which no delegated user token holds; and a **tenant-mode** user token holds neither `substrate:tenant` nor `substrate:user` (only *isolated* users carry `substrate:user`), so keying the gate on either scope would lock one user type out.

**A member-read gate, not a scope hole.** The user-read surfaces are gated on `runs:read` — the honest floor every such principal already holds (isolated users imply it, tenant-mode users hold it directly, tenant operators and admin sit above), so opening them grants no capability a user did not have. The security-bearing decision moves into the handler, keyed on the authenticated principal, never the wire.

**Own history is capped, not just admitted.** `/v1/_history` now admits a member token, and a delegated user's history scope is capped to `[self, user]` — its own chats, in both access modes — while a tenant operator keeps `[self, user, tenant]` and admin adds cross-tenant `global`. The owner subject/tenant is stamped from the principal, so `[self, user]` is exactly the caller's own history and nothing else. The cap is applied on both the HTTP and gRPC paths, so a member is confined identically on either transport. The TS `history()` method is unchanged — it already carried the caller's bearer.

**Discovery is tiered server-side by access mode.** A new `GET /v1/_runnable-agents` returns the agents the caller may run: bundled/system agents always (the shared floor), the tenant's shared agents only when the caller may use them, and own user-scoped agents (reserved — none today). An **isolated** token never sees the tenant's agents — a hard floor read from the token scopes, so a stale `access_mode` column can never widen it; a tenant-mode user is governed by its authoritative `users.access_mode` row. Entries are lean (`name` + `source` tier) — no operator metadata (version counts, retired badges, content hashes) and no system prompts; that stays in the `substrate:tenant` Library. A different tenant's agent is never leaked.

**`@loomcycle/client` 1.51.0** adds `runnableAgents()` (+ `RunnableAgent` / `RunnableAgentsResponse`). Additive; existing callers unchanged.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched. The gRPC/MCP discovery twins and a longer-term scope-hierarchy cleanup are noted as follow-ons in the RFC.

## What's in v1.50.0

**RFC BX Phase 2 follow-ups: an isolation invariant made structural, and the delegated-users API reaches `@loomcycle/client`.** Minor — additive adapter surface, one defense-in-depth runtime stamp, one CI deflake. No schema change, no wire change.

**The isolation bit is now stamped where it was merely unreachable.** RFC BX Phase 2 confines an isolated member (a `substrate:user`-topped principal) to its own data scope through `RunIdentity.Isolated`, which `ConfineIsolatedScope` reads to refuse tenant/global scopes. Every HTTP run-start site stamped it; the MCP-direct path (`mcpPrincipalCtx`) and the two gRPC substrate paths (`substrateGRPCCtx`, `substrateGRPCUserCtx`) did not. That was safe — but only because all three gate on `substrate:tenant`, a scope an isolated token cannot hold, so no isolated principal ever reached them. Safe-by-unreachability is a coupling between a route gate and a confinement in a different file: widen the gate and the confinement disappears silently. It is now stamped from `auth.IsIsolated(principal)` at all three sites, so the property holds by construction rather than by the current gate. Behaviour is unchanged today; the regression tests fail on the unstamped code.

**`@loomcycle/client` 1.50.0 gains the user + token management surface.** The RFC BX Phase 2 routes — `/v1/_users` CRUD and `/v1/_users/{subject}/tokens` mint/list/revoke — shipped with a Web UI console but no adapter method, so a programmatic operator had to hand-roll `fetch`. Added `createUser` / `updateUser` / `deleteUser` / `mintUserToken` / `listUserTokens` / `revokeUserToken` and their types, mirroring the Web UI client. The tenant is server-derived from the bearer, so none of them send one; the mint result carries the plaintext bearer exactly once. These are HTTP-only admin routes — the Python adapter is gRPC-only and there are no gRPC user RPCs, so it is unchanged.

**A CI flake removed.** `TestProviderGate_ZeroOverheadWhenUnconfigured` asserts five runs overlap in the provider (`peak > 1`) using a bare 20 ms delay, which a loaded runner could stagger into a false `peak=1` — it failed exactly this way on a recent `Go 1.26.x` job. It now uses the harness's existing `holdUntil` rendezvous so the observed peak is deterministic; a genuine under-admit still fails (the 2 s safety valve fires, peak stays < 2).

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.49.0

**🎯 Targeted memory search: ask for facts, notes, or documents by name.** RFC BW, all three phases, plus the `@loomcycle/memory-view` console package. One wire field changes value set — see the note below.

**The bug it fixes, measured.** A user asked their chat agent *"remind me which medicine do I use"*. Seven of ten `recall` hits were document chunks, and the fact naming their medication ranked **fifth** — below a horizontal rule (`"---"`, 0.477), a shell fence (`` "```sh" ``, 0.452) and a bare `#` (0.451).

Two independent causes, both ours. Markdown scaffolding was being embedded: a heading-split import turns a fence line into its own chunk, and a short syntax token embeds near the centroid of everything, so it ranks mid-high for *every* query — it does not waste a row, it buries answers. Those are now rejected, by one predicate covering both a chunk's body and its title fallback (a first attempt rejected the body and then fell through to an equally scaffold-ish title, moving the noise rather than removing it).

The structural cause was worse. **RFC BU §6 promised that `recall` does not reach documents** — and that held only because chunk bodies had no embeddings. RFC BU phases 1–2 embedded ~2,900 of them into the same per-scope vector plane `recall` searches, so the guarantee went false without a line of `recall` changing. The RFC that declared the invariant is the one that broke it, which is the argument for putting the boundary in the API rather than leaving it emergent from whatever happens to be indexed.

**A named selector, not another reserved string.** The reported failure was an agent not knowing that document bodies live under `doc.chunk:`, so answering it with a second magic string would have answered the symptom:

```
Memory op=recall scope=user query="which medicine"          → facts + notes (new default)
Memory op=search scope=user query="…" sources=[documents]    → document text
Memory op=search scope=user query="…" sources=[facts]        → distilled facts only
```

`prefix` survives as the escape hatch and composes as an AND. **`recall` now defaults to memory rather than everything** — that is the fix, not a side effect; an operator wanting the old behaviour passes all three sources explicitly. `search` still spans every plane by default, because `/v1/_memory/search` exists to answer "where did I record this" and narrowing it would break what v1.47.0 shipped.

**Facts vs notes is decided by `origin`, not `class`.** Both are provenance columns, but `origin` is stamped by the server from the writer's identity while `class` is model-supplied — so keying "is this a consolidated fact" off `class` would let an agent promote its own note to a fact by labelling it. Legacy rows predating the column count as **notes** rather than vanishing from both halves of the split.

**Two source combinations are refused rather than approximated.** `documents` together with only one of facts/notes needs a disjunction across two independent dimensions. It could be built and deliberately is not, because the alternative that matters is what happens instead: silently widening to "everything" hands back rows the caller excluded *while it believes the filter applied*. The error names the supported sets.

**A backend that ignores the selector must say so.** `sources_applied: false` is surfaced with a note, and false is the zero value on purpose — an external memory-layer service that drops the selector must not be able to do so silently, because the caller would then trust a label the result does not deserve. Recall is where it matters most: its default excludes documents, so a backend that ignored it returns exactly what the default exists to keep out.

**The filter runs in SQL, and that was forced.** The candidate pool is capped at 51 rows, so on a scope with ~2,900 chunks against ~42 facts a post-filter would leave a caller asking for ten facts with two.

**Measured before shipping**, because the RFC made it a gate: excluding documents on a 2,942-row scope is **~31× faster**, not slower — 30 ms against 928 ms. The reason matters more than the number: migration 0017 deliberately creates no ANN index, so there is nothing to degrade and a predicate cutting 2,942 candidates to 42 cuts the work proportionally. The risk returns if an operator opts into HNSW, which 0017 invites, so the regression assertion stays.

**Also: the Memory tool now teaches itself.** All three `chat/*` agents pointed at `Document graph_recall` for "what do we know about X" — which walks out from facts carrying entity metadata and returns `seeds: 0` on a store without them, reading as "nothing is remembered". Nothing named `Memory op=recall` or its required `scope`+`query`, so an agent guessed its way there through three missing-field errors. The prompts and the `memory-layer` help topic now name the invocation first and position `graph_recall` as the second step.

**New package: `@loomcycle/memory-view`** — the operator Memory console as a reusable React component (scope/scope_id/key browser, entry editor, fact viewer, unified search panel, reembed flow), scoped under `.loomcycle-memory-view`. The Web UI consumes it from source. Published on its own `memory-view-v*` tag; **0.1.0 is not published yet.**

### ⚠️ Wire change

`kind` on `POST /v1/_memory/search` was `"memory" | "document"` and is now **`"fact" | "note" | "document"`**. A reader switching on `"document"` is unaffected; one asserting `kind == "memory"` must move to `fact`/`note`. The refinement is the point — "the user told me this" and "an agent jotted this down" are different claims, and collapsing them is what let document prose read back as remembered fact. `@loomcycle/client` **1.49.0** moves in lockstep.

### Upgrade note

Existing deployments benefit immediately for new writes. Scaffolding rows already embedded stay in the index until re-swept; `POST /v1/_memory/backfill_embeddings` does not remove an existing embedding, so clearing them means deleting those chunks or re-embedding the scope.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.48.0

**🔖 Heading chunks became searchable, and `@loomcycle/client` ships the memory-view SDK.** Minor: one embedding-policy change and three additive adapter methods. No schema change, no wire/proto change.

**A bodyless chunk now embeds its TITLE.** Every embedding policy derived text from the *body*, so a chunk whose body yielded none was excluded from semantic search altogether — which silently removed the most navigable part of a document from retrieval. On the reference deployment **186 of ~3,000 chunks are heading-only**, with titles like `RFC BE — History Tool (browse / search / rename / annotate past chats)` and `Phase 2 — name-links + transclusion`.

The rule applies uniformly after the per-type switch: when the derived text is empty, fall back to the title. It is a **fallback, never an addition** — appending it would double-weight whatever the author put in the heading on every chunk in the corpus, and for prose the body usually restates the heading anyway. Being uniform, it also covers a diagram whose labels were all syntax and an image with neither caption nor description, which would have made the deployment's two images findable before any vision call.

The only quality test is **"contains a letter"**, not a length. Measured before building: of 20 sampled bodyless chunks 18 had meaningful titles, and the shortest were `Active RFCs` (11 characters) and `Configuration` (13) — a length filter would discard exactly what someone searching a document would type. `---`, `42`, `1.2.3` carry no language and are dropped, so no vector is stored for them.

The admin backfill applies the same fallback, or existing documents would stay permanently less searchable than ones authored after this landed — and a bodyless chunk is unreachable any other way, since the sweep sees memory *rows* while a title lives in SQL Memory. Both surfaces route through one judgement so they cannot disagree.

**`@loomcycle/client` 1.48.0** adds three HTTP-only methods backing the memory view: `memorySearch()` (the off-run unified search over k/v entries and document-chunk bodies from v1.47.0, each hit tagged `kind: memory|document`), `memoryEmbedStats()`, and `reembedMemory()` — whose `dryRun` stays a safe dry run when omitted, since the server defaults it true. Fact reads (`list_facts`, `get_chunk`'s `entity` block) ride the existing `document()` passthrough rather than gaining bespoke methods. Additive: existing callers are unchanged.

**Why npm skips 1.47.0.** The adapter's version was bumped to 1.47.0 in the same window that tag was cut, and a publish only fires when the package version equals the tag — so `@loomcycle/client@1.47.0` never existed on npm. It is realigned to 1.48.0 here, and npm goes 1.46.0 → 1.48.0. Nothing is missing; there was never a 1.47.0 package.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched; the SDK methods are TS-only because the endpoints they wrap have no gRPC twin.

## What's in v1.47.0

**🔎 A human-facing memory view, the memory surfaces opened to tenant operators, and the last of the RFC BU sweep fixes.** Minor rather than a patch because RFC BV Phase 1 adds new surface — one HTTP endpoint and two Document ops — and the `/v1/_memory/*` family changes who can reach it. No schema change, no wire/proto change, no adapter change.

**RFC BV Phase 1 — reading the memory plane like a human would.** The entity tier stores a fact as a chunk plus a `chunk_memory_meta` sidecar (bi-temporal timelines + provenance), but nothing could *read* that sidecar in a typed way: `get_chunk` returned a chunk with no way to tell a fact from a plain section, and nothing enumerated facts for a browse surface.

- `get_chunk` now attaches an **`entity`** block when the chunk has a sidecar (omitted otherwise): raw unix-nanos timestamps, so the viewer formats them rather than the server guessing, and an always-present `retired` bool keyed on *system* time (`expired_at`) so a future world-time `invalid_at` is not misreported as retired.
- **`list_facts`** browses the scope's facts newest-first, metadata only — the viewer fetches a body on click — filterable by type/class/document_id and using the same temporal filter `graph_recall` does. Its per-fact `entity` block reuses the same renderer, so the two surfaces cannot drift.
- **`POST /v1/_memory/search`** is an off-run semantic search with an empty key prefix, so one query spans both plain k/v entries and document-chunk bodies — what answers "where did I record this" across the whole stack. Each hit is tagged `kind=memory` or `kind=document` (+`chunk_id`) so a document hit can be followed to `get_chunk`.

  Security-critical detail: the in-process backend resolves the tenant from the *run* identity, and off-run there is none — so the handler stamps the authenticated principal's tenant before searching. Without it the search would run at the shared `""` tenant and could read another tenant's rows. `TestMemorySearch_TenantIsolation` fails if the stamp is removed.

**The `/v1/_memory/*` family is reachable by tenant operators.** It was pinned to `substrate:admin` by the `/v1/_*` catch-all, so a `substrate:tenant` operator 403'd at the gate — even though memory rows carry a `tenant_id` and every handler already sources the tenant from the authenticated principal rather than the wire. The same gap the Library had in v1.6.3: the handler confines, but the route never lets a tenant token reach it. The routes now grant `ScopeTenant` and the handlers still confine a non-admin to its own tenant, with `TestHandleMemory_TenantOperatorConfined` proving a `?tenant=` naming another tenant is ignored — the invariant the re-gate rests on.

**`repair-tenant` deliberately stays operator-admin**: it rewrites rows across every scope in one statement to re-stamp the legacy `""` partition, and a cross-tenant bulk rewrite is not a tenant operator's authority. The Web UI's memory nav item moves from admin to tenant with it; **channels stays admin**, because it still has no tenant column — the reason memory used to be admin too.

**An uncaptioned image could never be embedded.** Found by verifying the v1.46.1 deploy rather than trusting its output: the describe pass reported `described=2 failed=0` with two accurate descriptions persisted, and the images stayed unsearchable while the backfill's candidate count never moved.

`embedBody` guarded on the **raw body** before the per-type switch — and an image's body *is* its caption, so an image with no caption returned early and its generated description was never consulted, no matter how many times a describe pass wrote one. The body is only one of the sources for an image, and may not be a source at all.

This is the failure mode `SetAssetDescription`'s own comment warns about, reached by a different route: `get_asset` shows a description, the sweep reports success, the row holds 472 characters — and nothing is indexed. Correct-looking from every surface an operator would check. Both existing image tests used a *captioned* chunk, which is why it went uncovered, and an uncaptioned image is the common case for an uploaded asset. The fix derives the text first and checks it after; prose and diagram behaviour is unchanged.

**Upgrade note.** If a describe pass already ran on v1.46.0/v1.46.1, its descriptions are persisted and need no second vision call — rewriting each image chunk's body re-enters the embed path and indexes it. The embedding backfill *cannot* do this: it reads the chunk-body envelope from the memory plane and has no view of `chunk_assets`, so an uncaptioned image is invisible to it.

Adapters unchanged: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.1

**🩹 Both v1.46.0 upgrade sweeps were broken in practice.** Patch release — no new surface, no schema change. Found by running the sweeps against a real deployment (154 documents, 3,143 chunks, 2 images); neither fault was visible from the unit tests, because each needs scale or a real model to appear.

**The embedding backfill starved instead of finishing.** Throughput decayed 189 / 179 / 169 / 160 / 147 embedded per 200-row window, heading for zero with ~2,300 rows still unembedded. `limit` bounded how many rows were *looked at*, not how many were embedded — and a row with no body text (a document root, a section heading) can never gain an embedding, so it stays a candidate forever. Because the query is `ORDER BY key`, those rows accumulate at the *front* of the window: the residue obeys `R' = R + p·(limit − R)`, whose fixed point is `R = limit`. Eventually the window is entirely rows that cannot be embedded and the sweep does nothing, while its own notes promise "re-invoke until candidates reaches 0" — a state it can never reach. `MemoryEmbedListMissing` now takes a keyset cursor and the handler pages with it until it has *embedded* `limit` rows, reporting `skipped_empty` and `more`.

Widening the window did not help either: the store reset a limit over 1000 back to the 200 default, so asking for 5000 returned a **smaller** page than asking for 1000. That is now a clamp.

**The describe pass truncated before it said anything.** The image sweep reported 0 failures and both images as answered-empty. The model was fine — asked directly, `qwen3.6` describes the image in 198 characters. The token ceiling was 300, and a thinking model emits its reasoning trace *first*: 1281 characters of it, `done_reason=length`, and no description at all. 300 looked ample because the same prompt answers in 84 tokens with thinking off.

Worse than a failed call: the pass recorded it as answered-empty and stamped `described_at`, whose whole purpose is to separate "a model looked and found nothing" from "nothing has looked yet" — so a code bug became a permanent fact about the data that no re-run would revisit. A turn that stopped at the ceiling is now a **failure**, left retryable, with the reason named. The ceiling is 1500, and it is a ceiling rather than a target, so a generous value costs nothing on a normal call. Deliberately *not* fixed by forcing `effort: low` (which the Ollama driver maps to `think:false`): that flag errors on a model which cannot reason, so it would break a non-thinking vision model such as llava to accommodate a thinking one.

**Also:** an admin token that names no tenant on the backfill is refused with `400 tenant_required`, mirroring the erasure, directory and orphan-repair surfaces. Memory rows are keyed on the tenant, so omitting it resolved to the *default* tenant and reported a truthful-looking `candidates: 0` against a tenant the operator never meant — demonstrated on a three-tenant deployment, where the same request with and without `?tenant=` returned 200 candidates and 0.

**Upgrade note.** If you already ran the v1.46.0 sweeps, re-run the backfill — it will now finish rather than stall. Images stamped by the buggy describe pass need clearing to become candidates again:

```sql
UPDATE chunk_assets SET described_at = NULL
 WHERE described_at IS NOT NULL AND coalesce(description,'') = '';
```

Adapters unchanged from v1.46.0: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.0

**📄 Documents became a searchable, linked knowledge store.** Two RFCs completed end to end — **RFC BS** (structure primitives: tags, links, transclusion, history, discovery, canvas) and **RFC BU** (searchable bodies: embed-on-write per chunk type, backfill, diagram and image handling) — plus a **directory** surface on every transport. No schema migration on the main store; the document scopes self-provision their own tables.

**Documents are now findable by their content.** They were not, and the reason was structural rather than a bug: the searchable half of a document (the SQL `chunks` table) has no body column, while the half holding the text (`doc.chunk:<id>` rows in Memory) had no index — `writeBody` called plain `MemorySet`. The keyword leg did not rescue it either, since full-text ranks a column on the *embeddings* table, so an unembedded row was invisible to both search paths. Neither component was faulty; the gap was in the seam, which is why neither one's tests could surface it. Chunk bodies are now embedded on write, and `memory op=search` with prefix `doc.chunk:` is the entry point.

What gets embedded is **per chunk type**, and the rule that fell out of it is worth stating: *use a model only when the content is not already text.*

| chunk type | embedded text | model? |
|---|---|---|
| prose | the body verbatim | no |
| `mermaid` | extracted node/edge labels + the diagram kind | no |
| `image` | the author's caption + a persisted vision description | for the description only |

Mermaid is text pretending to be a picture; an image is a picture. A diagram's labels are extracted deterministically across ten dialects — and the extractor is two passes, because a first version using label-shape regexes alone silently lost the content of half of them (`erDiagram` entity names are bare identifiers, `gantt` task names sit *before* the colon while the metadata follows it, `mindmap` nodes are bare indented words). An unrecognised diagram type degrades rather than vanishing.

For images, the **caption is embedded on write with no model at all**, so an image is searchable the moment it is written. A vision description is generated by an explicit operator pass (`POST /v1/_document/describe_images`, tier-resolved, dry-run by default, resumable) and **persisted** rather than regenerated: a description is model output, so regenerating yields different text, and an index that silently re-ranks on every re-embed is worse than one that is merely stale. Persisting it also makes it auditable and survives an embedder swap without a second vision call per image.

**Prose became a graph.** A chunk body's `[[name]]` links are materialised as `references` edges, re-derived on every body write and resolved through the Path dirent tree; `![[target]]` **embeds** are expanded inline at export time (transclusion), degrading to the literal text on a cycle, a depth cap or an unresolved target — a rendering nicety must never abort an export. Plus the discovery half: **`backlinks`** (what links here, manual and parser edges alike), **`related`** (semantic neighbours, a straight reuse of the new body embeddings), **`unlinked_mentions`** (chunks that name a target without linking to it), and a per-chunk **body-change log** with `history` / `get_version` / `diff`.

**Tags and document-level type/status** are first-class query axes now, in their own SQL join tables. A chunk's `fields` live in a Memory k/v blob, unreachable from SQL, so a tag stored there could never be queried — and "every draft RFC" previously meant querying root chunks. **JSON Canvas** import/export round-trips a document to the open spatial-graph format Obsidian Canvas uses.

**A directory surface**, on HTTP, MCP, gRPC, TS and Python: `users`, `inspect`, `tenants`. Read-only and derived — there is deliberately no create or update, because a "user" here is not stored (`ListUsers` is a `GROUP BY` over `runs.user_id`), so "user CRUD" has no create/update half and its delete half is already the subject-erasure surface. What was missing was the *read*: answering "what does alice actually have here" meant five calls against five surfaces, each with its own tenant-scoping rule, where getting one wrong yields not an error but a plausible number from the wrong tenant. Listing tenants is admin-only and **refuses** rather than filtering, because a filtered list still confirms the caller's own tenant in a shape indistinguishable from "you are the only tenant here".

**Two bugs worth calling out**, both found by building on the code rather than by review:

- A mermaid chunk was embedded as **raw diagram source** when created natively, while the identical diagram arriving through `import_md` was skipped. The classifier recognised only the fenced form, but a mermaid chunk *stores its bare source* — so the common path was the broken one. Behaviour now branches on the authoritative chunk type, since no content predicate can separate "a diagram" from "prose that opens with the word pie".
- The describe route calls `provider.Call` **directly** without the credential-override path, exactly like the LLM gateway — so without the same refusal it would have been a cost-isolation bypass, letting an operator-key-restricted tenant spend the operator's key. It now returns 403 `operator_key_restricted`.

**Also:** the memory extractor runs at `effort=low` **in the copy that actually ships** (the fix was in a bundle file that was not the embedded one), and an `effort=medium` evaluation baseline is recorded under the current prompt.

**Upgrade notes.** Two one-time sweeps make *existing* content searchable, and neither runs automatically by design — thousands of model calls at boot is a bill nobody approved:

```
POST /v1/_memory/backfill_embeddings?scope=user&scope_id=<subject>&prefix=doc.chunk:&dry_run=false
POST /v1/_document/describe_images?scope=user&scope_id=<subject>&dry_run=false
```

Both are resumable with no cursor: an embedded row or a described image drops out of its candidate set, so re-invoke until `candidates` reaches 0. Every chunk write now makes an embedder call — best-effort, so a cold or absent embedder logs and moves on rather than failing an author's write. Expect per-scope vector growth roughly equal to the chunk count, which brings the existing per-scope quota closer.

**The shared document viewer surfaces all of it.** `@loomcycle/explorer` gains read-only tag chips, a **Connections** panel with three lazily-fetched lists (backlinks — marking which edges came from `[[name]]` parsing — plus related and unlinked mentions, the three reads under `Promise.allSettled` so one failure never blanks the others, and a missing embedder showing a muted "not configured" note rather than an error), a **History** modal with the revision list, one revision's exact body and a unified diff, and a **Download .canvas** button beside Download .md. Board, kanban and spatial views stay a loomboard concern.

Adapters: `@loomcycle/client` **1.46.0**, `loomcycle` (PyPI) **1.46.0** — the Python adapter had been pinned at 1.38.0, so this release publishes its erasure and directory methods — and `@loomcycle/explorer` **0.6.0**.

## What's in v1.45.0

**🧹 Subject erasure on every transport, and a nine-pass audit of the memory subsystem.** One new wire surface; the rest is fixes. Additive index only (0065, from v1.44.0); no schema or wire break.

**Erasure is no longer HTTP-only.** v1.44.0 shipped it as the one substrate surface without transport parity; it now runs over MCP (an `erasure` tool, `op=report|execute`), gRPC (`ErasureReport` / `ErasureExecute`), the TS client (`erasureReport()` / `erasureExecute()`), and the Python client. The logic moved into a shared service so all four run the SAME code — an erasure that removed a different set of planes depending on which client asked would make "we erased them" a claim about a library rather than about the data. **The safety guards moved with it**: requiring `confirm` to equal the subject is a property of the erasure, not of HTTP, and leaving it to each caller would mean the newest transport is the one missing it. On MCP and gRPC the tenant comes from the principal with no wire field at all, so a session cannot reach another tenant's subject.

**Then nine review passes over the memory subsystem, which found fourteen defects.** Every one sat in state reconstruction, tenant keying, or reachability — none in transport, dispatch or gating. The ones an operator should know about:

- **A single-tenant deployment's erasure silently spared the subject's entire SQL Memory database** — every document they authored and their whole entity graph. SQL Memory rejects an empty tenant and stores the default one as `"default"`, while the k/v plane keeps the raw `""`; the drop key was built from the raw value and matched nothing. The live deployment test could not have caught it: its tenant was non-empty, where the two spellings coincide.
- **The correction chain could fork.** Nothing stopped two chunks from each superseding the same fact — both stayed live and `graph_recall` returned both as current, which is exactly what supersede-not-delete exists to prevent.
- **A known future end date deleted the fact.** "The contract runs until 2027" vanished from every default recall the moment the end date was written, because the filter required `invalid_at IS NULL`. The decisive argument is internal consistency: the default *is* "as_of now", so it must reduce to the `as_of` predicate with the current time substituted — and it did not.
- **`import_md` read fenced code as document structure.** Any document containing a Markdown sample re-imported with more chunks than it had, its body truncated at the fence. That is most technical documentation, including this project's own RFCs.
- **`create_chunk` / `move_chunk` accepted a parent that does not exist**, returning success for a chunk nothing could ever reach — absent from `get_document` and `export_md`, and invisible to the dead-link sweeper, which looks for a missing *document* rather than a missing *parent*.
- **Vector search died instead of degrading in a mixed-dimension scope** — the state migration 0017 calls the typical steady state mid-migration. One row at another dimension aborted the whole query with a raw driver error, so recall was dead for that scope until every row was re-embedded.

**Three hardening fixes where a guarantee held only by accident.** The postgres schema and LOGIN role for a SQL Memory scope are derived by hashing a separator-joined key, and the separator-free requirement was asserted in a comment rather than enforced — three distinct scopes could derive one schema and one role. It was unreachable only because the tenant component happens to come first. The SQL validator's comment-stripper was dialect-blind, so a legitimate `VALUES ($$x; y$$)` was refused while `$$--$$` hid a real statement separator — safe only because the driver's extended protocol rejects multi-statement strings, a dependency nobody had written down. And a `MemoryBackendDef` could name any env var as its credential source: `api_key_env: LOOMCYCLE_AUTH_TOKEN` beside a def-supplied `base_url` is a one-request exfiltration of the operator bearer, and the def persisted holding it.

**⚠️ Two CI gaps, and the second explains several of the above.** The pgvector round-trip contract **never ran** — it is gated behind an opt-in flag the workflow never set, even though the store job's service image is already pgvector. A skipped suite is worse than a missing one: it reports green and reads as covered. It could not have run even with the flag, because the per-test schema excluded `public` and migration 0017's `vector` type is unqualified. Both are fixed, and the postgres-tier `-run` filter — which the workflow *asks in writing* to be maintained — is now enforced by a test rather than a comment.

**🔬 One eval finding.** A change to the extractor's system prompt **un-gates every measured configuration**, invisibly: the baseline keys on the prompt hash, and no match is deliberately not a regression. `effort=medium` has been ungated since the prompt changed. The gate now says so instead of reporting a clean pass with nothing behind it.

**Adapters:** `@loomcycle/client` **1.45.0** publishes on this tag with the erasure methods. The Python client's erasure methods ship on its own `python-v*` tag.

## What's in v1.44.1

**🩹 Patch — the erasure report omitted planes it had examined.** Reporting accuracy only; nothing about what gets deleted changes.

Found by driving v1.44.0 against a live deployment rather than a fixture. A dry run for a real subject returned `{"chats":2,"interrupts":0,"memory_rows":2}` — but `credentials`, `token_limits`, `path_entries` and `sql_memory_scopes` are all examined on that path, and all four were missing. An operator reading that cannot tell *"this subject had no credentials"* from *"credentials were never looked at"*, which in a compliance report is the entire question being asked. It is the same ambiguity between FAILED and EMPTY that the `errors` field exists to prevent, reintroduced one field lower down.

Two causes. The executor accumulated with `deleted[k]++` from an absent key, so a plane that removed nothing left no trace; every considered plane is now registered at zero up front, which establishes the invariant that **a key's presence means the plane was examined and its value means how many rows went**. And the report set `sql_memory_scopes` only when it found one, so absence meant either no scope or no look.

SQL Memory is the one genuinely conditional plane: with the subsystem unconfigured it really is unexamined, so rather than omit it the report now SAYS so in `notes`. Exactly one of the two must hold — counted, or declared unexamined — and the regression test asserts that as an exclusive-or rather than checking either alone.

⚠️ **No schema change, no wire change, no adapter change.** Existing `/v1/_erasure` responses gain keys; none are removed or renamed.

## What's in v1.44.0

**🧹 A subject erasure you can run, and — more to the point — one that tells you what it did not reach.** Two new endpoints, one new store method, one additive index. The memory bundle stays opt-in.

**`GET /v1/_erasure?tenant=&subject=` reports one person's footprint in three tiers**, and the tiers are separated by what they *guarantee* rather than by what they contain:

| tier | what | deletable |
|---|---|---|
| 1 | chats, user-scope memory, the user's SQL Memory scope, path entries | ✅ existing primitives |
| 2 | credentials, token limits, interrupts, usage ledger | ❌ nothing, before this release |
| 3 | facts about the subject in a shared agent's or another user's scope | ❌ not addressable at all |

Tier 2 is listed apart because the distinction is not row count but whether anything can remove them. A subject's **encrypted credentials** surviving an "erasure" is the worst entry on that list, and it is invisible unless something counts it.

**`POST /v1/_erasure` executes tiers 1 and 2, and defaults to doing nothing.** `dry_run` is a nullable boolean on purpose: an omitted field — or `POST {}` from a client with a buggy serialiser — must mean *do nothing*, and a plain boolean would make the zero value destructive. A live run additionally requires `confirm` to equal `subject`; a subject id that matches nothing is harmless, one that matches the **wrong person** is not. An admin token must name the tenant explicitly, because a subject id is only unique within one and an unnamed tenant would not merely report the wrong people — it would delete them.

⚠️ **The residue report is one-shot, and this is the property to understand before using it.** Tier 3 is reachable only by tracing provenance from the subject's chats, and the erasure deletes those chats. Afterwards the trace handle is gone: a report run later shows `residue: 0` **while the facts are still stored**. That is not a defect to be fixed at this layer — it is what "the subject's chats are deleted" means when chats are the only index into derived facts. So the residue is measured before anything is removed, the response states that **it is the only durable record of what was not reached**, and the report closes the matching lie from its own side by rendering tier 3 **UNDETERMINABLE** rather than `0` when there are no sessions to trace from. That is exactly the state a subject is left in after an erasure, and exactly when someone would read a zero as confirmation the job is done. **Retain the erasure response.**

**The usage/cost ledger is retained by design, and says so.** Cost rows are accounting records an operator may be legally required to keep; the correct treatment is to break the personal linkage rather than destroy the totals. That is a row merge rather than a delete — `usage_archive` carries `user_id` inside its primary key, so anonymising means folding rows into the empty-string bucket and summing — and it is left for its own change rather than swept in. It appears under `retained` with the reason, because an erasure that lists only its successes reads as complete.

**Deleting an interrupt is not filtered by status.** An interrupt row holds the model's question *and* the user's free-text answer, so removing only the pending ones would report success having left the conversation behind. The new `InterruptDeleteAllByUser` is also uncapped, unlike the lister it mirrors: a partial delete that reports success is the one outcome an erasure must never produce.

**🧹 Dead chunk references are now collected.** A chunk is referenced from five places, and neither `delete_chunk` nor the read-time guard reaches all of them — the body delete runs after the transaction commits (a different store, so it cannot join the txn), an out-of-band delete bypasses the tool entirely, and the read-time guard makes an orphan *invisible*, which is precisely why nothing ever notices it. This is always-on integrity rather than opt-in policy: everything it removes is unreachable by definition. Two guards refuse rather than proceed — a fault reading the live chunk set aborts the scope, and a scope holding bodies but zero chunk rows is treated as mis-resolved rather than fully deleted, because being wrong there costs every body in the scope.

**🔬 The memory eval never sent the ontology it was scoring.** `{{memory:ontology}}` reached the model as nineteen literal characters, which explains the v1.43.0 finding about entity types outside the ontology: the model was never shown one. The A/B rerun settles it — **a confirmed ontology eliminates statement-class-as-entity-type entirely (5 → 0)**, retiring the type-validation follow-up that release proposed.

⚠️ **One additive migration (0065)** — a partial index on `memory(tenant_id, source_session_id)`. No wire change, no adapter change. The erasure surface is **HTTP-only in this release**: there is no MCP tool, no gRPC RPC, no adapter method and no Web UI page, unlike every other substrate surface. An operator drives it with `curl`.

## What's in v1.43.2

**🔎 The entity graph was correct and unfindable.** The first live consolidation pass built a graph, and inspecting it showed why the retrieval half was unusable. Runtime + bundle; no migration, no wire change.

**`graph_recall` searched every chunk in the scope, and the scope is where the document store lives.** Measured on the reference deployment: **2 entity chunks among 3,071 chunks across 150 documents**. So "what do we know about X" answered with documentation prose. Discovery now requires a `chunk_memory_meta` row — a chunk without one is prose, not part of the graph — and it is restricted for DISCOVERY only, never for explicit `seed_ids`, which exist precisely to hand in results found some other way. That is the same division the temporal filter already draws.

This is the failure mode a fixture corpus cannot show. Every test corpus is a clean room; signal-to-noise is a property of the corpus rather than the code, and 2-of-3071 is only visible where the other 3,069 exist.

**The title match was a bare substring, so `statin` matched `re-statin-g`.** Verbatim from the deployment: a query for "statin" returned a configuration paragraph reading "an overlay that puts OAuth on top WITHOUT restating the base matrix". Whole-word matching now happens in Go, because neither storage tier has a portable word boundary and the padded-LIKE trick misfires on punctuation — a title ending in a colon, or a hyphenated term, would stop matching. The LIKE remains a coarse prefilter and the fetch over-fetches, so filtering can never silently return fewer seeds than were asked for, which would read as "the graph holds nothing else" rather than "the prefilter was noisy". A query with no word character at its edges falls back to substring instead of matching nothing.

**A statement class is not an entity type.** The pass typed "the user prefers statin alternatives" as `preference:user` — the entity of type *preference* named *user*. That is not a harmless mislabel: **every preference about the user collapses onto that one node**, producing a hub that means nothing and burying the facts it should organise. An invented type such as `process` makes one strangely-labelled node; a statement class makes a magnet. `preference` and `fact` are now refused as entity types (they belong to the extractor's `class` vocabulary, and are in the ontology because the memory tier needs them as chunk types), and the refusals are COUNTED — a pass that mirrored nothing because every type was rejected must not look like a pass with nothing to mirror.

⚠️ **A claim in the v1.43.0 notes was wrong, and is corrected there.** Those notes said the extractor's inconsistent subject spelling forked one person into two graph nodes. It does not: `slug()` lowercases and its stopword list already drops `a`/`an`/`the`, so `the user`, `user` and `The user` all reduce to one key. The claim had been read out of the code rather than run — and it surfaced because the fix written for it changed nothing when removed, which is only possible if it was doing nothing. The regression test is kept regardless, because the guarantee is load-bearing and currently emerges from a stopword list nobody would think to protect: deleting the articles from it forks every subject whose name carries one.

**Still accepted, and stated rather than hidden:** entity types OUTSIDE the ontology (`profession`, `setting`, `process`, `task` all appeared in the eval). Refusing them needs the effective ontology reachable from the consolidator, which by design cannot read a tenant-scope document — a follow-up, and far lower stakes than the magnet above.

**🧰 Deploy fixes for the serve-and-test posture.** The sandbox sidecar pin is bumped so `expose` actually works: #891 set `SANDBOX_EXPOSE_NETWORK` but left the sidecar on an image predating the expose seam it added in the same change, so the old sidecar silently dropped the unknown field, opened a plain session and returned no exposed host — a serve-and-test run "succeeded" while the browser could not resolve the alias. The rest of the family had drifted too. And the TrueNAS deploy gains a **browser + sandbox variant**: build a feature in an isolated sandbox, serve it, and test it in a headless browser in one run, which was previously reachable only in the cloud deployment.

⚠️ **No migration, no schema change, no wire change.** Adapters unchanged. The memory bundle remains opt-in with its schedule at `enabled: false`. If you have already run a pass, entity nodes typed `preference:` or `fact:` from before this release are still in the store — they are inert rather than harmful (nothing reads them as entities now) but worth deleting if you want a clean graph.

## Earlier releases

Notes for **v1.43.1 back to v0.4.0** are not repeated here.

They were dropped from this file deliberately rather than lost: every tag carries
its own annotated message, and every release has a GitHub page with the same
notes. This file had grown to 185 sections and ~700 KB, which made it useless for
its actual job — telling you what changed *recently*.

- **Per-release notes:** <https://github.com/denn-gubsky/loomcycle/releases>
- **From a checkout:** `git tag --sort=-v:refname` to list, `git show <tag>` for one
- **What a version contains:** `git log --oneline <older>..<newer>`

For the shape of the runtime as it stands rather than how it got there, see
[`README.md`](README.md); for where it is going, [`docs/PLAN.md`](docs/PLAN.md).
