# LoomCycle release history

Per-version release notes from v0.4.0 onward. The current and immediately previous releases are also summarised in the main [`README.md`](README.md); older releases live here.

For the **public roadmap** (planned v0.8.16 through v1.0 work — Question tool, Pause / Resume / Snapshot, distribution, operator postures), see [`docs/PLAN.md`](docs/PLAN.md).

## What's in v1.47.0

**🔎 A human-facing memory view, and the last of the RFC BU sweep fixes.** Minor rather than a patch because RFC BV Phase 1 adds new surface: one HTTP endpoint and two Document ops. No schema change, no wire/proto change, no adapter change.

**RFC BV Phase 1 — reading the memory plane like a human would.** The entity tier stores a fact as a chunk plus a `chunk_memory_meta` sidecar (bi-temporal timelines + provenance), but nothing could *read* that sidecar in a typed way: `get_chunk` returned a chunk with no way to tell a fact from a plain section, and nothing enumerated facts for a browse surface.

- `get_chunk` now attaches an **`entity`** block when the chunk has a sidecar (omitted otherwise): raw unix-nanos timestamps, so the viewer formats them rather than the server guessing, and an always-present `retired` bool keyed on *system* time (`expired_at`) so a future world-time `invalid_at` is not misreported as retired.
- **`list_facts`** browses the scope's facts newest-first, metadata only — the viewer fetches a body on click — filterable by type/class/document_id and using the same temporal filter `graph_recall` does. Its per-fact `entity` block reuses the same renderer, so the two surfaces cannot drift.
- **`POST /v1/_memory/search`** is an off-run semantic search with an empty key prefix, so one query spans both plain k/v entries and document-chunk bodies — what answers "where did I record this" across the whole stack. Each hit is tagged `kind=memory` or `kind=document` (+`chunk_id`) so a document hit can be followed to `get_chunk`.

  Security-critical detail: the in-process backend resolves the tenant from the *run* identity, and off-run there is none — so the handler stamps the authenticated principal's tenant before searching. Without it the search would run at the shared `""` tenant and could read another tenant's rows. `TestMemorySearch_TenantIsolation` fails if the stamp is removed.

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
