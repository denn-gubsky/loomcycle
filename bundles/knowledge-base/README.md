# knowledge-base bundle

The links agent. `kb/linker` builds and maintains the relations (statements)
between a document's passages and the rest of the store, using only rules the
tenant's ontology declares. A model-judged relation is only ever proposed, and
never as `elaborates`, `conflicts_with` or `stricter_than`: read by hand on real
corpora, those were almost never right, so they are reported and not filed.
The bundle also carries the annotation pass, which writes a brief and an
abstract for each section, document and folder of the tenant knowledge base
(see [Annotations](#annotations)), and the agents that answer questions about
it (`kb/ask` and the agents it runs).

| agent | provider | tools | role |
|---|---|---|---|
| `kb/linker` | `code-js` | `Document`, `Agent`, `Channel` (subscribe `documents/ingested` and `memory/subjects` only), `Context` (`op=time` only, for L4's time budget) | the deterministic driver: owns the pass, every check and every write |
| `kb/verdict` | your `low` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one bounded relation verdict per call, for L4 only |
| `kb/judge` | `code-js` | `Document`, `Agent`; `statement_methods: [judge]` | rules on the proposals the review policy routes to the judge (see [The judge](#the-judge)) |
| `kb/judge-verdict` | your `middle` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one verdict per proposal: accept, reject or unsure, a confidence and one sentence |
| `kb/annotator` | `code-js` | `Document`, `Agent`, `Channel` (subscribe `documents/ingested`, read only); tenant scope; `statement_methods: [annotate]` | writes the annotations of the tenant knowledge base (see [Annotations](#annotations)) |
| `kb/annotate` | your `middle` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one part in, `{brief, abstract, language}` out |
| `kb/taxonomist` | `code-js` | `Document`, `Agent`; tenant scope; no `statement_methods` | proposes taxonomies (concept schemes) from the document briefs, filed as one inert proposal (see [Taxonomies](#taxonomies)) |
| `kb/taxonomy-writer` | your `middle` tier, `effort: low`, `max_tokens: 32768`, `max_context_tokens: 131072` | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one domains, generate, update, align or review request in, one JSON object out |
| `kb/ontologist` | `code-js` | `Document` (read mode); tenant scope; `statement_methods: [ontology]` | reports which entity types and relation kinds the corpus uses and the ontology does not declare, ranked as a propose pass would file them; report mode only in this release, no model call and no write (see [Building the ontology](#building-the-ontology)) |
| `kb/ontology-writer` | your `middle` tier, `effort: low`, `max_tokens: 4096` | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one class or relation candidate in, `{verdict, parent, meaning}` out; shipped for the propose mode to come, not called yet |
| `kb/ask` | `code-js` | `Decision`, `Agent`, `Document` (read; `describe_ontology` only, no scopes) | the entry point for a question about the tenant knowledge base. A question bounded by a date or a version ("since v1.0", "since 9 November", "opened after 2026-11-09", "in the last 10 days") goes straight to `kb/walker` with no classifier asked and a fixed note to list the documents by creation date (`query_documents` `created_after`), which the survey's searches cannot filter by; it reports `CLASSIFIER: dated`. "since" with no date or version after it ("since when", "since the tool is read-only") does not count, and a date bound counts only after a question word or a list verb at the start of a clause, in the same sentence, so a date in a situation told before the question ("I moved to the UK in 2019. Can I claim X?") does not count. So does a question naming a period by both ends ("in November 2026", "during March", "in 2026", "between 1 May and 1 June 2026"), with a note to bound both ends (`created_after` the start, `created_before` the first instant after the period), a month with no year read as the latest such month that has begun (the walker says which year it took), and a `TODAY: <date> (UTC).` line, the run's own date, after it; a month or a year alone needs "in" or "during" before it and the end of the clause after it, so "what may change", "the march of versions" and "in May release notes" do not count. A question asking to list the documents of a kind by stored status ("which of our RFCs are still drafts", "which RFCs are confirmed but not done", "the draft plans", or with the status as a verb after a subject: "which RFCs has the owner confirmed", "which plans have we finished", "which design notes are not yet marked done") or every document of a kind ("which implementation plans do we have", "list all design notes", or by what we wrote: "which plans have we written", "which RFCs did we draft", "which guides have been published", never with another subject: "which plans has the owner written" does not count) also goes to `kb/walker`, reported as `CLASSIFIER: listing`, with a fixed note to list with `query_documents` by status (one call per status asked, named on a `STATUS: <statuses>.` line: "not done" is every other status, and "confirmed but not done" is `confirmed` alone) or by type, `limit` 1000, to list every class of the kind the ontology declares (the type filter includes subtypes, never a sibling class), and to name every row. The kinds are the tenant ontology's document classes, read with one `describe_ontology` call and only for a question that starts a clause with a question word or a list verb: each class's name said as words (`DesignNote` is "design note", plural "design notes"; `Rfc` is "rfc") and each of its `@alt` names ("playbook" for a `Runbook`). A kind resolves to its class and to every class whose name ends with it ("research" is `Research` and `TechnicalResearch`), less a class whose ancestor is also named, and the note ends with a `CLASSES: <names>.` line naming them, to pass to `type` exactly as written. The root `Document` class and the operational types (`SourceStub`, `SyncReport`, `Annotation`) never make a listing. With no confirmed ontology, a failed read, or no document class but the root, kb/ask uses a fixed list instead (RFCs, design notes, designs, implementation plans, research, guides, how-tos, runbooks, specs, proposals, measurements, reports, papers, publications, notes) and the note names no class. A kind narrowed by a topic ("which design notes cover the scheduler", "which plans do we have for speech"), the status of one document, and "what do we hold on X" do not count. A question that is both dated and a listing is one walk with both notes, reported as `CLASSIFIER: dated+listing`. Depth `quick` on a listing shortens each line, never the list. Otherwise it asks whether the question wants an overview. A question asking for *the* overview, guide, walkthrough or full picture of one thing ("give me the full picture of how X is put together") is a known item and goes to `kb/search` with no classifier asked, unless it also asks for the list ("everything", "all", "documents", "what do we hold"); its partial search answer stays with search. Otherwise a Decision call first (0.5 or more: yes; 0.1 or less: no), then `kb/classify` for a score in between or a failed call (run metadata `classifier: agent` asks `kb/classify` only). An overview runs `kb/survey`, anything else `kb/search`; a `COVERAGE: partial` search goes on to `kb/walker` with the ids found, unless the Decision score was 0.1 or less or `kb/classify` said no with no Decision score beside it, and so does a search that did not complete. A question given as untrusted input is passed to each child by reference, still untrusted. A walker's answer (the dated or listing route, or the walk after a search) is returned without its `MOVES:`, `MISSING:` and `COMPOSE:` lines or any sentence about them, the same strip `kb/survey` applies. Optional run metadata `depth` (`quick`, `standard`, `deep`; see [Retrieval depth](#retrieval-depth)) says how much to retrieve. Optional run metadata `first_call: true` (the boolean or the string `"true"`) sends a question bound for `kb/search` to `kb/answer` first (see [The first call](#the-first-call)). Ends with `ROUTE: … \| SCORE: … \| CLASSIFIER: … \| RUNS: …`, then `\| FIRST: … \| SECTIONS: … \| RERANKED: …` when the first call ran, then `\| DEPTH: <depth> (caller\|wording)` when the depth is not standard |
| `kb/classify` | your `low` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one question in, `YES` or `NO` out; an unreadable answer counts as an overview |
| `kb/answer` | `code-js` | `Document` (read), `Agent` (spawns `kb/read` only); tenant scope; `memory_rerank: {enabled: true}` | the first call of `kb/ask`, opt-in, and usable on its own: one passage search of the tenant scope (20 hits, no annotations, reranked), `get_chunk` on the hits in order until 5 sections with text (a repeat and a hit with `has_body: false` skipped), one block of them (`S1`…`S5`, the chunk id, the document and section title, the text; each field folded onto one line as `kb/survey`'s `BRIEFS`, each text cut at 6,000 characters, the block held to 30,000) handed to `kb/read` as an untrusted block, then code checks the reply: citations are kept only as an index in `1`…`K`, anything else (`S0`, an index past `K`, a copied id) is dropped and counted. Returns the answer, a `Sources:` list of the cited sections as links, and `FIRST: found\|not_found\|no_sections\|read_failed \| SECTIONS: <k> \| RERANKED: true\|false (<reason>) \| DROPPED: <n>`; a `not_found` or `read_failed` answer lists the sections read instead |
| `kb/read` | your `middle` tier | none (`tools: [-Calc]`, `skills: [-*]`, `disable_context: true`); `temperature: 0`, one iteration | one question and its sections in; `ANSWER: <answer>` (the answer first: yes or no, or the shortest phrase, plus at most one condition) or `ANSWER: NOT FOUND`, then `SOURCES: S<n> …` by index, never an id |
| `kb/search` | your `middle` tier | `Document`; tenant scope | flat search with summaries; ends with `COVERAGE: complete` or `COVERAGE: partial` |
| `kb/survey` | `code-js` | `Document` (read), `Agent`; tenant scope | the path for overview questions. Code gathers the candidate documents (a passage search, a summary search, a mixed search, two searches per clause of the question, then one hop of accepted relations from the 60 highest-weighted), ranks them (a search list counts 2, a top-10 hit 1 more, a relation 1) and keeps the top 120. A document of 15 or more live chunks is a large guide: its hit sections are candidates beside it, at most 6 per guide, each picked best match first (the best rank a hit in them held in any gather search), never by weight. `kb/screen` screens the candidates' ids, never their text: the documents first, in slices of 12, then the sections of documents that did not fit, at most 60 best match first, in slices of 20 beside the documents' retries (the matching sections of a fitting document, at most 60, are not screened: they are linked beside it in the answer); five rounds at a time (`max_concurrent_children: 5`), each given the exact read calls for its slice's ids (one `documents_summary` call, one `get_annotation` call per section), reading their briefs and replying with only a `FITS:` and a `SET ASIDE:` line. A round that did not complete, stopped at the output cap, or lacks either line counts as failed and nothing it said is used; its slice (and any ids a good round left out) is screened once more as two halves, never a third time. One more `kb/walker` round then composes the reply from the ids that fit, given only those ids, each fitting document's matching sections to link under it, each fitting document's stored type and status (one-word labels code reads with `documents_summary`; the stored status overrides a brief that names an older one), and the briefs themselves, which code reads (`documents_summary` in calls of at most 24 ids, one `get_annotation` per section) and hands over as data in an untrusted block of their own (`BRIEFS`, one row per id, each row at most 1,200 characters and an even share of 48,000 for the block), so the round makes no call to compose; its `MOVES:`, `MISSING:` and `COMPOSE:` lines are stripped with the accounting lines, and so is any sentence that talks about them (one naming `MOVES`, `MISSING`, `COMPOSE`, `FITS` or `SET ASIDE` in capitals outside a link, unless it cites a document), while the rest of its line stays; if nothing fits, code says so with no composing round, and if the composing round fails, code lists the fitting documents and sections as links. Code adds its own coverage line: `COVERAGE: complete` only when every candidate was sorted, no document was cut by its cap nor a section by either cap of 60, no guide had matching sections past its limit of 6, the size count did not fail, and at least as many candidates were kept as the passage search's facet window counted documents (`documents_counted`); else `COVERAGE: partial` and `UNCOVERED: <n> candidates not screened; <m> beyond the cap of 120`, with `; <j> sections beyond their cap of 60`, `; <g> matching sections beyond the per-guide limit of 6` or `; the sections were not counted` when that is why, and `; the passage search counted <c> documents and <k> were kept` when fewer were kept, or `; the searches may not have reached every matching document` when the search reported no count; sections never count toward the documents kept. The caps above are depth `standard`; [Retrieval depth](#retrieval-depth) gives the others. Ends with `SURVEY: candidates … (cut …) \| sections … (cut …, over guide limit …, document fits …) \| slices … \| failed slices … \| rounds … \| depth … \| RUNS: …`, where a run cut by the output cap shows as `<run>:completed/max_tokens`. A gather step that fails or finds nothing falls back to a plain walk, reported partial |
| `kb/screen` | your `middle` tier | `Document` (read); tenant scope | sorts one slice of `kb/survey`'s candidates by their briefs and replies with only the `FITS:` and `SET ASIDE:` lines; a short prompt of its own, so a screening round does not carry the walker's |
| `kb/walker` | your `middle` tier | `Document`, `Path`; tenant scope | composes `kb/survey`'s reply from the ids that fit; without a list, walks the document tree by its summaries (the fallback after a partial search) and, for "what changed (is new, grew) in X" where the ontology declares topics, measures which topics' share moved between two periods (`measure` `compare`), then reads the members of the two or three that moved most (`search` `concept`) that the period's dated listing (`query_documents` `created_after`/`created_before`) holds; with no series stored, the dated listing alone |

`kb/search`, `kb/survey`, `kb/screen` and `kb/walker` only read: they hold
`Document` (and the walker `Path`) in read mode (`tool_modes`), so the runtime
refuses any write op before it runs, whatever a stored document tells the model
to do. `kb/ask` holds `Document` read only too, with no memory or SQL scope, and its code calls only `describe_ontology`. `kb/survey` hands `kb/screen` and the walker document and section ids only: a title, passage or brief is document text,
and is never pasted into a trusted prompt. The composing round's `BRIEFS` block
is the one place document text reaches a child from code, and it goes as an
untrusted block: the runtime fences it and escapes every spelling of `<`, and
code first folds each stored field onto one line (no line break, no control or
invisible character, `<`, `>` and `&` as look-alikes), so no brief can close the
fence or start a row of its own. Both the round's prompt and the walker's say
the block is document content, never instructions and never tool calls.
`kb/answer` hands `kb/read` its sections the same way: one untrusted block,
each field folded onto one line by the same helper, and a fixed trusted
prompt; a question given as untrusted input goes by reference, still
untrusted. `kb/read` has no tools, and its citations are indexes that code
maps to chunk ids. `kb/ask` holds no grant for any of this: the rerank applies
only to the searches of an agent that enables it, which is why the first call
is an agent of its own.

## The first call

With run metadata `first_call: true`, `kb/ask` answers a passage question in
one shot before it starts a search loop. A question it would send to
`kb/search` (a known item, or anything the classifiers did not call an
overview) goes to `kb/answer` instead; dated, listing and overview questions
route as before. Without the flag `kb/ask` answers byte for byte as it did.

| `kb/answer` says | depth `standard` | depth `quick` |
|---|---|---|
| `found` | its answer is returned | the same |
| `not_found` | goes on to `kb/search` | its answer is returned |
| `no_sections`, `read_failed`, or its run did not complete | goes on to `kb/search` | the same |

Going on, `kb/search` is handed the ids of the sections already read (never
their text), and the search route's own rule then applies unchanged: a
partial or failed search goes on to `kb/walker` as above. Depth `deep` never
takes the first call. The route line names the steps
(`ROUTE: answer`, `ROUTE: answer -> search (not found)`,
`ROUTE: answer -> search (not found) -> walker (partial coverage)`), keeps
its four fields in order, and adds
`| FIRST: <outcome> | SECTIONS: <k> | RERANKED: true|false (<reason>)`
before any `DEPTH` field. These describe the first call even when a later
step answered; `FIRST: none` means its run gave no readable result.

## Retrieval depth

An agent that calls `kb/ask` and does not need the full overview can ask for
less with run metadata `depth`:

| `depth` | `kb/survey` | the search route |
|---|---|---|
| `quick` | at most 30 documents screened (3 rounds of 12, one wave, no retry wave), no sections of large guides, relations from the 15 heaviest candidates, 4 clause searches, and a composing round asked for short lines | a partial search answer is kept (no walk after it); the answering agent is told to give the few best documents |
| `standard` (default) | the caps in the table above | a partial answer goes on to `kb/walker` as described above |
| `deep` | the same screening caps, relations from the 100 heaviest candidates and 8 matching sections per large guide (code reads and linked lines, no extra screening round) | a partial answer always goes on to `kb/walker`; the answering agent is told to widen until the counts are covered |

The caller's value wins. Without one, `kb/ask` picks `quick` when the question
itself asks for little ("briefly", "in short", "a quick list", "a short
summary", "top 5"); otherwise `standard`. `kb/ask` passes a non-standard depth
on to its child as one fixed trusted line (`DEPTH: quick. …`), which
`kb/survey` reads from trusted text only (a `DEPTH` line inside an untrusted
question sets nothing) and takes out of its searches; `kb/survey` called
directly also reads run metadata `depth`. A quick survey that left anything out
says so: documents past its cap of 30, a large guide's matching sections and the
ids of a round that failed (it is not retried) are counted in `UNCOVERED`, and
its coverage is `partial`. The `SURVEY` trailer always names the depth used.

A listing question (`CLASSIFIER: listing`) is complete or it is wrong, so its
`quick` line asks for one short line per document listed and no reading beyond
the listings, and still for every document; the route line still ends
`| DEPTH: quick (…)`. A dated question without a listing gets the ordinary
`quick` line, as before.

## What it needs

- SQL Memory.
- The `statement_methods: [rule, extractor]` grant, which the bundle declares.
  It lets the linker stamp `rule:*` and `extractor:*` methods on its
  statements and hold the linker lease. Without it, a `propose` run fails at
  its lease.
- For L4 (relate): an embedder on the Postgres vector tier, and a `low` tier
  for `kb/verdict`.

Under `LOOMCYCLE_ACL=enforce`, the access rules of whoever starts the linker must
allow these verbs on the documents it works on:
- `propose`, to file statements, take the lease, and verify, flag or re-point
  evidence;
- `link`, on both ends' documents (and on the survivor's, for a merge), for
  `maintain` mode to expire, demote or merge.

Without `link`, those three actions are refused one by one, and the report
counts them under `refusals`.

## Layers

One pass covers one document. The layers run in this order:

| layer | reads | writes (`propose` mode) |
|---|---|---|
| **L0 classify** | the runtime's `classify` op on the document: the ontology's class rules (`@title_pattern`, `@identifier_pattern`), its Instances, and the neighbours' classes | in `propose` mode the op writes: exactly one class matched by its own rule is applied (type, identity fields, so the work key, and an `issued_by` an adopted instance can take); anything else is proposed for an operator, and a type set by hand is kept. `report` mode asks with `write: report`. The linker never accepts a proposal: that is an operator's act. With `file: {root}` a class in force also files the document: it gets a name at `<root>/<Class>/<title>` |
| **L1 anchors** | each live part's anchor and stable id | nothing. It reports parts without an anchor, duplicate anchors (`…~2`, by id) and parts without a stable id |
| **L2 cite** | the citations the document class's `@citation_grammar` finds | one statement per citation, from the citing passage to the cited part, `method: rule:citation`. A citation to a held work whose part is missing points at the work's root; one to an unheld work gets a placeholder. The grammar's `kind` is used only when the ontology declares it and it is not reserved; otherwise the statement is `cites` |
| **L3 terms** | the concepts of the scheme named by `terms_scheme`: their name and alternative labels | `uses_term` from each passage to each concept it mentions, `method: rule:terms`; or `defines`, `method: rule:definitions`, for a term a definitions passage defines (see below). Matches are exact or case-folded with whitespace collapsed, on word boundaries, and the longest match wins. **Tenant scope only**, because concepts live there; at user or agent scope the matches are counted and nothing is written |
| **L4 relate** | semantic neighbours from `related` (of `target_types` when given, never a copy or version of the document), judged by `kb/verdict` | in `propose` mode, a kind the tenant's ontology declares itself (see below): one proposal per passage and target document, with both quotes as evidence, by `extractor:relate`. `elaborates`, `conflicts_with` and `stricter_than` are reported, never filed. It runs only when `corpus_paths` is given or `L4` is listed |
| **L5 check-against** | each passage's norms under `check_against`: first the parts of the works it already cites, then the closest by meaning, up to `max_norms` (12); one `kb/verdict` call per passage | **a report document, no statements**: `<report_root>/<title>-<date>` (default root `/reports/check-against`), each finding (`addresses`, `complies_with`, `possible_gap`, `possible_conflict`) with both quotes, under a *not a legal opinion* banner. It runs only when `check_against` is given or `L5` is listed |
| **L6 maintain** | every live statement with an end in the document, or with evidence citing it | see below |

L2 and L3 need a confirmed ontology with relation kinds. Without one they skip
and the report says why. L0 needs a confirmed ontology, with or without relation
kinds. L1 never needs it; L4 judges without one, but offers and files only kinds
a confirmed ontology declares. L6 still runs, without its kind and redundancy
checks.

`defines` is a seed kind. `uses_term` is not: declare it in the ontology's
Relations section (`### uses_term`). Until then, L3 files only `defines`, and
counts its other matches as `hits_no_kind`.

### L0: filing

With `file: {"root": "/board/by-class"}`, a document whose class is in force gets
a name in its class's home, `/board/by-class/<Class>/<title>` (the title reduced
to letters, digits, `.`, `_` and `-`; the document id when nothing is left).
Filing **adds** a name through `set_path`: the document keeps every name it
had, so a folder tree you arranged is never moved, and `Path rm` takes the added
name away. A name another document already holds is left to it: the document's
id is appended to its own. Without `file`, nothing is filed. The report says
which class a document was filed under (`layers.L0.filing`), never the path,
which carries the title.

### L3: which passage defines a term

A class's `@definitions` pattern marks its definitions passages: the runtime
flags each one `definitional` when the linker reads the document's chunks. In
such a passage, a term defines its concept when it starts the body or a line,
after at most indentation, a list marker (`1)`, `1.2.`, `а)`, a bullet) and an
opening quote. So in "1) договір — угода …" the passage defines договір; in "У
цьому Законі договір …" it only uses it.
- Each concept the passage defines gets one `defines` statement, with its
  defining occurrences first in the evidence. The passage's other terms get
  `uses_term`.
- If the ontology doesn't declare `defines`, those terms get `uses_term`
  instead, counted as `defines_not_declared`.
- Terms are matched in the passage's body, so a term that appears only in a
  chunk title is not seen.

### L4: what reaches a verdict

A passage–candidate pair costs a verdict call only after these filters, each
counted by name in `layers.L4.counts`:
- **Target classes.** With `target_types` (document class names), a candidate
  is kept only when its document is of one of them or of a subclass by the
  ontology's class tree (`candidates_not_target_type`). Name the classes that
  hold your norms, for example `["Law", "Regulation", "Policy"]`. Without
  `target_types`, the classes a confirmed ontology marks with `@norm` (and their
  subclasses) are the targets, read from `describe_ontology`'s `norm_classes`.
  With no `@norm` anywhere, a partial ontology read, or a runtime too old to
  report `norm_classes`, every document under `corpus_paths` is a candidate, as
  before. The report's `target_types_from` says which applied: `input`,
  `ontology` or `none`.
- **Not a copy of the document.** A candidate document is dropped when it is in
  the document's copy family (`candidates_copy_family`): the other end of a
  `duplicate_of`, `alternate_of`, `revises`, `revision_of` or `version_of`
  statement with one end in the document, either way, asserted or proposed; or
  a document with the same title once copy markers (`Copy of `, ` (1)`,
  ` - Copy`, …) are stripped, the rule `find_duplicates` uses. If the family
  cannot be read, the pass fails rather than judge without it.
- **A passage, not structure.** A passage or candidate is skipped when it is
  only headings (`chunks_skipped_heading` / `candidates_heading`; one short line
  with no sentence counts as a heading), mostly contents lines with dot leaders
  and page numbers (`…_toc`), mostly two-cell label rows with no figures, the
  shape of a rating legend (`…_legend`), or has fewer than `min_passage_words`
  words (`…_few_words`, default 8). A price table has figures, so it is judged.

What the verdict may answer:
- The kinds offered are the five built-ins (`elaborates`, `implements`,
  `stricter_than`, `conflicts_with`, `alternate_of`) plus every relation kind
  the tenant ontology declares itself (`describe_ontology`, source `tenant`),
  with its meaning, its inverse and its domain/range, up to 32 in all
  (`tenant_kinds`, `tenant_kinds_over_cap`). A reply naming any other kind is
  counted as `verdicts_out_of_vocabulary` and dropped. A caller's `relations`
  list replaces the whole vocabulary, tenant kinds included.

### L4: what a relation proposal needs

A verdict becomes a proposal only when all of these hold:
- its kind may be filed: never `elaborates`, `conflicts_with` or
  `stricter_than` (`relate_withheld_kinds`; counted as `kind_withheld`, and the
  finding carries `may_file: false`, `not_filed: "kind_withheld"`); otherwise a
  kind the tenant ontology declares itself, or a built-in kind the bundle lists
  in `relate_propose_kinds` (none today, so `implements` and `alternate_of`
  stay report-only, `propose_kind_off`); and the ontology declares it writable
  (`propose_kind_not_declared`). Every finding stays in the report, filed or
  not;
- both quotes occur verbatim, the passage's in the passage and the candidate's
  in the candidate. There is no confidence floor: on a real corpus, confidence
  did not separate right verdicts from wrong ones;
- neither quote lies in a paragraph the document's class marks with
  `@boilerplate` (`quotes_in_boilerplate`). Those paragraphs are also left out
  of what the verdict is shown; a passage with too little left is skipped, and
  a candidate with nothing left is not sent;
- each quote has at least `min_quote_words` words (default 4,
  `quotes_too_short`), is not the chunk's title or a heading line
  (`quotes_heading`), and is not a contents line or a legend row
  (`quotes_toc_or_legend`);
- it is the best-supported finding for its passage and target document: the
  highest confidence, then the candidate's rank. The others are counted
  (`propose_capped`) and stay in the report's `findings`.

A proposal is always `proposed`, by `extractor:relate`, with the verdict's
confidence and both quotes as evidence. An operator's rejection stands: the
runtime refuses to file a rejected relation again.

### L5: check-against

`{"document_id": …, "check_against": ["/board/regulatory-external"]}` compares
each passage of an internal document with the norms it answers to. A finding
needs both quotes verbatim and outside boilerplate, like an L4 one. The findings
are written to a report document, one section per passage that has any, and the
document opens with a banner: the findings are a model's, checked only for their
quotes, and **not a legal opinion**. Nothing is filed in the statement graph. A
report pass compares and counts, and writes no document. A report name already
taken that day gets `-2`, `-3`, ….

### L6: maintaining what exists

For each statement, the first check that fails decides the outcome:

1. **Endpoints.** An end chunk that no longer exists: expire.
2. **Kind.**
   - A kind the ontology renamed: the statement is proposed again under the new
     name, then the old one expires.
   - A kind no longer declared: demote it to `proposed`.
   - An end outside the kind's `@domain` or `@range`: demote it to `proposed`
     (a statement that is only proposed is flagged instead). The source is
     compared with `@domain` and the target with `@range`, by the end chunk's
     own type: a document's class for a document, the chunk's type for a
     subject. A subclass of an allowed class is inside it, `entity` accepts any
     subject and `concept` a concept of the ontology's schemes, as when the
     relation was written. The write-time check does not run again when a
     document is re-classified or the kind's ends are narrowed; this one does.
     A statement is left alone and counted `unchecked` when its kind declares
     neither end, or when an end has no type or a type the class tree does not
     hold (a class renamed or removed since): the pass does not demote on a
     class it cannot place. The classes are read in one query per 100 distinct
     ends of a document's statements, and only for kinds that declare an end.
3. **Validity.** An end replaced by a newer edition: expire it, after proposing
   it again on the successor (`attributes.revision_of`) when it still holds there.
   The new proposal keeps the original's `valid_at`, but not its confidence.
   - **A citation** (`rule:citation`) is re-read by the document's citation
     grammar on the new part. It moves when a hit there still cites the same work
     with the same kind, with that hit's quotes as its evidence: a citation that
     lost its number form but kept its title, or whose line broke elsewhere,
     still moves. One the new part no longer makes is not moved, even if an old
     quote survives word for word (the new edition may name a newer act in the
     same words), and the copy the re-import made of it is expired too. If the
     grammar cannot be read, the rule below applies.
   - **Any other statement** moves when every quote it had on the old part is
     still in the new one. A quote whose words survive with different line
     breaks or spacing counts, and the new evidence quotes the new part's own
     text. A statement with only some of its quotes left is not moved: what is
     left cannot vouch for what the passage now says.
4. **Evidence.**
   - A quote no longer in its chunk but present in the live part with the same
     stable id: the evidence moves to that part.
   - Otherwise the evidence row is marked stale.
   - When all of a statement's evidence is stale, it is demoted.
5. **Redundancy.** Statements left identical once a kind dropped its
   discriminator are merged into the oldest of the same status. A duplicate of
   another status is flagged.
6. **Semantics** (only with `rejudge: true`). A model-made statement
   (`extractor:*`) that passed 1–5, of a kind the relate vocabulary knows, is
   judged again by `kb/verdict` when its passage's or its target's body was
   rewritten since it was last verified (or filed). Nothing that did not change
   costs a model call.
   - The same kind, same direction, with quotes that occur: verified.
   - A kind the relate layer may file: proposed as a revision of it
     (`propose_statement` with its `id`), and the statement is flagged.
   - Anything else (none, an unquotable answer, a kind L4 may not file):
     flagged.

   A second opinion never expires or demotes a statement: a person decides. A
   person's statement, or one an operator ruled on, is never re-judged.

If every check passes, the statement is verified.

These rules always hold:
- A statement a person wrote (`manual`, or a method the linker does not know),
  or one an operator ruled on, is only ever **verified or flagged**. Statements
  written by `rule:`, `extractor:`, import and canvas are all maintained the
  same way.
- A synced statement is judged by the method that first made it
  (`attributes.origin_method`). One with no `origin_method`, synced before the
  field existed, may be a person's, so it is only verified or flagged.
- A statement the runtime wrote (the edition chain's `supersedes`, the
  consolidator's links, `[[name]]` links) and any reserved kind are **not
  touched**.
- An auto link whose text is gone from its source is counted
  (`auto_rule_a_candidates`), and nothing else happens to it.
- Nothing is ever deleted.

## Modes

| `mode` | over | layers by default |
|---|---|---|
| `document` (default) | `document_id` or `path` | L0, L1, L2, L3, L6 (+ L4 with `corpus_paths`) |
| `maintain` | `document_id` or `path` | L6 |
| `drain` | `queue: documents`, the `documents/ingested` queue (at `scope: user` or `tenant`); `queue: subjects`, the `memory/subjects` queue (at `scope: tenant`); up to `max_messages` | L0, L1, L2, L3, L6 per document message; L6 per subject message |
| `subject` | the subject's document (`document_id` or `path`), whose root is the subject | L6 |
| `sweep` | the statements with an end under `path` (a Path subtree) | L6, then the purge report |

| `write` | does |
|---|---|
| `report` (default) | Issues read ops only, takes no lease and acks nothing. It counts what it would write in `would_write`. **Run this first.** |
| `propose` | Takes the scope's `linker` lease first; a busy lease means `status: busy` and nothing else. Renews the lease as the pass goes and releases it on every exit path. Writes through `propose_statement` and `maintain_statement` and never asserts anything itself: the runtime decides auto-assertion. |

**Drain** reads one message at a time without consuming it:
- Each message is acked only after its pass is recorded.
- A malformed message, or one naming a document the scope no longer holds, is
  acked and reported as such.
- A failed pass, or one cut by the budget, stops the drain and leaves its
  message unacked. Acking any later message would consume the failed one.
- A `reimported` message runs L2 and L3 on its changed parts only. L6 still
  covers the whole document.
- A document message names the scope it was published at. The bundle declares
  `documents/ingested` `publisher: system`, as it does `memory/subjects`: only
  the runtime writes it, and an agent's publish is refused even with a publish
  grant. So a message naming another scope than the drain's can only mean the
  deployment declares the channel at another scope than its drain reads. The
  drain stops there with `status: failed`, the message unacked
  (`scope_mismatch`), and `failure` says which declaration to change. A scope
  the runtime never publishes at is malformed (acked, never a stop), and a
  message from before the runtime named its scope is processed as usual.

Each queue is declared at one scope and names that scope's documents. `queue`
picks the queue; without it the scope does (`user` → `documents`, `tenant` →
`subjects`):
- `queue: documents` drains `documents/ingested`, the documents imports
  created or changed. The runtime publishes it in the document's own scope.
  The bundle declares it at `scope: user`, so `scope: user` reads a user's
  imports. A deployment whose documents live in the tenant scope re-declares
  it at `scope: tenant` (below) and drains it with
  `{"scope":"tenant","queue":"documents"}`.
- `queue: subjects` (at `scope: tenant` only) drains `memory/subjects`, the
  subjects the runtime announces when one is adopted, merged, re-typed, removed
  or erased. Adopted subjects live in the tenant scope; a merge is an operator's `POST /v1/_ontology/subjects/merge`
  (or its gRPC, MCP and client twins), which has already re-pointed the scope's
  statements by the time the message arrives. An `erased` message comes from
  erasing a user that a subject is bound to (`POST
  /v1/_ontology/subjects/principal`); it names the subject, never the user, and
  the subject is kept. Each message is one subject pass (L6 only):

| `op` | the pass |
|---|---|
| `adopted`, `retyped`, `erased` | checks the statements touching the subject |
| `merged` | re-points what remains on the duplicate to the survivor (`surviving_document_id`), then checks the survivor |
| `removed` | nothing: the scope's statements went with the document (`nothing_to_check`) |

A subject message is trusted for its ids only; the pass re-reads the subject and
its statements under the linker's own grants. `scope: tenant` needs the tenant
grants on `kb/linker` (see [Scope](#scope) below). `scope: agent` drains nothing and is
refused, as are `queue: subjects` at `scope: user` and `queue` outside drain
mode.

**Subject** checks every statement touching a subject, for a curator to run after
changing one. With `merged_into` (the surviving subject's document id), each
statement with the subject as an end is filed again with the survivor in its
place (same kind, evidence and date, `attributes.revision_of` the original) and
then expired; a person's statement, or one an operator ruled on, is flagged
instead. The other statements touching the subject get the ordinary checks.

To link documents on import, add an opt-in schedule. Import latency is then the
schedule's interval:

```yaml
scheduled_runs:
  kb-linker-drain:
    agent: kb/linker
    schedule: "*/10 * * * *"
    user_id: me                       # whose documents/ingested queue to drain
    concurrency_policy: forbid        # the lease would refuse a second one anyway
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","write":"propose"}' } ]
  kb-linker-subjects:                 # the subject queue: needs the tenant grants
    agent: kb/linker
    schedule: "*/10 * * * *"
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","scope":"tenant","write":"propose"}' } ]
```

A deployment that imports into the tenant scope (every document under one
tenant, not per user) re-declares the ingest queue at tenant scope in its own
config layer, which merges over the bundle and keeps its bound and semantic, and
drains it with `queue: documents`. The drain needs the tenant grants on
`kb/linker` (see [Scope](#scope)):

```yaml
channels:
  documents/ingested:
    scope: tenant                     # the bundle declares user
scheduled_runs:
  kb-linker-ingest:
    agent: kb/linker
    schedule: "0 3 * * *"             # daily: import latency is up to a day
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","scope":"tenant","queue":"documents","write":"propose"}' } ]
```

Or as a runtime schedule, over MCP `scheduledef` (or `POST /v1/_scheduledef`):

```json
{"op": "create", "name": "kb-linker-ingest",
 "overlay": {"agent": "kb/linker", "schedule": "0 3 * * *", "concurrency_policy": "forbid", "enabled": true,
   "prompt": [{"role": "user", "content": [{"type": "trusted-text",
     "text": "{\"mode\":\"drain\",\"scope\":\"tenant\",\"queue\":\"documents\",\"write\":\"propose\"}"}]}]}}
```

A drain reads at most `max_messages` (default 10, up to 100) per run, so a
daily drain behind a busy import day needs a larger `max_messages` or a shorter
interval. Run it once with `write: report` first.

**Sweep** checks the statements with an end under `path`, asserted ones first,
each least recently verified first, up to `max_statements`; `sweep.exhausted`
says whether it reached the end. What it verifies moves to the back of the
order, so repeated sweeps work through a large subtree without a cursor to
carry. A statement already flagged for a person is passed over
(`skipped_flagged`) until they rule.

Every sweep then reports, under `purge`, the retired statements a purge may
delete: expired, made by code, retired more than `purge_after_days` ago (default
180). It deletes them only when the run says `purge: true` (with `write:
propose`); never from a schedule you did not write that way. A purge never
deletes a person's statement, one an operator ruled on, or a **rejected** one: a
rejection is what keeps the linker from proposing that relation again.

A sweep schedule is not installed by the bundle. To add one:

```yaml
scheduled_runs:
  kb-linker-sweep:
    agent: kb/linker
    schedule: "0 3 * * *"             # nightly
    user_id: me
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"sweep","path":"/docs","scope":"user","write":"propose","max_statements":500}' } ]
```

## Use

```bash
LOOMCYCLE_PRESETS=base,knowledge-base ./bin/loomcycle
```

```bash
curl -sX POST localhost:8080/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"agent":"kb/linker","user_id":"me",
       "prompt":"{\"path\":\"/acts/incident-reporting\"}"}'
```

| field | default | meaning |
|---|---|---|
| `mode` | `document` | `document`, `maintain`, `drain`, `subject` or `sweep` |
| `document_id` or `path` | exactly one (none in drain; sweep takes `path` only) | the document; for a sweep, the subtree |
| `scope` | `user` | `user`, `agent` or `tenant` (drain: `user` or `tenant`) |
| `queue` | by scope | drain only: `documents` (`documents/ingested`, at `user` or `tenant`) or `subjects` (`memory/subjects`, at `tenant`); `user` → `documents`, `tenant` → `subjects` |
| `layers` | per mode | any of `L0`, `L1`, `L2`, `L3`, `L4`, `L5`, `L6`; `maintain` takes `L6` only |
| `write` | `report` | `report` or `propose` |
| `terms_scheme` | none (L3 skips) | the concept scheme L3 matches |
| `as_of` | each part's own date | unix nanoseconds; citations resolve as of it |
| `max_writes` | `500` | cap on writes per run, also counted in report mode (1–5000) |
| `max_statements` | `500` | cap on statements L6 checks per document (1–5000) |
| `max_messages` | `10` | messages a drain processes (1–100) |
| `corpus_paths` | — | L4 only: where its candidates come from |
| `exclude_document_types`, `candidate_floor`, `max_candidates_per_chunk`, `max_verdict_calls`, `max_chars`, `min_body_chars`, `relations`, `include_quotes` | as before | L4 only. `min_body_chars` also bounds which passages L3 reads; `max_verdict_calls` and `relations` also bound and name what L6's `rejudge` asks |
| `target_types` | the ontology's `@norm` classes, else every document under `corpus_paths` | L4 only: the document classes a candidate may come from, subclasses included |
| `min_passage_words`, `min_quote_words` | `8`, `4` | L4 only: a passage or candidate with fewer words is not judged (0–200); a quote with fewer words is no evidence (1–50) |
| `time_budget_seconds` | `1500` | L4 only: the wall clock after which the pass stops between verdict calls, `budget_stopped` (10–86400). Keep it below the agent's `run_timeout_seconds` (1800) |
| `rejudge` | `false` | L6 check 6: judge a model-made statement again when its text changed |
| `merged_into` | — | subject mode: the document of the subject this one was merged into |
| `purge` | `false` | sweep mode, with `write: propose`: delete what the purge report lists |
| `file` | — | `{"root": "/…"}`, with L0: file a document whose class is in force under `<root>/<Class>/` |
| `check_against` | — | L5: the Path-tree paths of the norms a passage is compared with |
| `max_norms`, `report_root` | `12`, `/reports/check-against` | L5 only: norms per passage (1–20); where the report documents go |
| `purge_after_days` | `180` | sweep mode: how long a statement stays retired before a purge may delete it (1–3650) |

An unknown field or an out-of-range value refuses the run with
`{"status":"refused","reason":…}`, before any tool call or lease.

## The report

The run's final output is one JSON object:

- `status`:
  - `complete`;
  - `budget_stopped`: `max_verdict_calls` or `time_budget_seconds` was reached;
    `budget.stopped_by` and `layers.L4.stop_reason` say which. The report keeps
    every verdict judged before the stop. The time budget is read from the
    `Context` tool (`op=time`) before each verdict call; without the tool the
    pass runs under the run's own cap and `layers.L4.clock` is `unavailable`;
  - `busy`;
  - `failed` (`failure` says why). This includes a pass in which no write went
    through and at least one was refused for a reason other than "already
    accepted" or "rejected". Such a refusal usually means a configuration fault,
    such as a missing method grant. It also includes an L4 pass whose verdict
    calls all failed, or more than half of at least three
    (`layers.L4.verdicts_failed`): its counts are kept, but no findings would
    not mean the document relates to nothing.
  - `refused`.
- `ontology_version`: the version the pass ran under. `ontology` shows whether
  it was usable and why not.
- `lease`: whether a lease was taken, whether it was acquired, `held_by`,
  `renewals`, `lost` and `released`.
- `layers.L1` … `layers.L6`: each layer's `status` (`run`, `skipped` with
  `reason`) and its `counts`.
  - A layer that proposes counts each write by what the runtime made of it:
    `proposed` or `asserted` (a new statement), `evidence_added` (a new quote on
    a statement already held), `unchanged` (statement and quote both held), or
    `already_asserted`. A re-run over the same text proposes nothing: its writes
    count as `unchanged`.
  - L6 adds `checks` (which check decided) and `actions` (per action).
    `checks.domain_range` counts the domain and range check: `checked` (every
    end the kind constrains could be placed, or one is outside), `mismatched`
    (of those, an end outside),
    `flagged` and `demoted` (what was done about a mismatch; in report mode,
    what would be), and `unchecked`. It replaces the `domain_range_unchecked`
    counter.
  - L4 keeps its findings and spans in UTF-16 units (`span_unit`). Each finding
    says whether a propose pass files it (`may_file`, and `not_filed` with the
    reason when not). L4 also reports `withheld_kinds`, `tenant_kinds`,
    `target_types`, `target_types_from`, `copy_family_documents`, `stop_reason`, `elapsed_ms` and
    `clock`.
- `writes`, `would_write`: per op. `refusals`: per op and refusal code.
- `budget`: the caps and what was spent.
- `queue` and `messages` (drain only): the queue read, and each message's
  outcome and whether it was acked. A document message carries its `reason`, a
  subject message its `op`. `scope_mismatch` is a document message of another
  scope, left unacked.

Every evidence span the pass sends is in code points (`evidence_span_unit:
"rune"`). The report carries ids, counts and fixed reason strings, never store
text. L4 quote text appears only with `include_quotes: true`.

## The judge

The tenant's review policy (the ontology's Review section, see
[DOCUMENTS.md](../../docs/DOCUMENTS.md#review-policies)) can leave a kind for
a judge instead of a person: `judge kind:elaborates floor:0.9`. `kb/judge` is
that judge. Per routed proposal with no verdict yet, newest first, it reads what
a reviewer would see — both ends' documents and passages, the evidence quotes
read back from the chunks they cite (never the proposal's own copy), the kind's
meaning, domain and range — asks `kb/judge-verdict` once, and hands the verdicts
to the Document op `judge_statements`. A `subject` proposal with no quotes (a
classifier model's, from `classify`), or whose only quote is the document's
title (`node_stats` assign and misfits, methods `rule:assign` and
`rule:centroid`), is judged on the document's opening
instead — its root and first sections, about 2,000 characters — beside what
the topic means; any other proposal without evidence is skipped
(`no_evidence`). The runtime decides what each does:

- it **rules** (asserts or rejects, `ruled_by: judge:<run id>`, no operator
  mark) only a proposal the policy routes to the judge, at or above that rule's
  floor (`floor:`, else the section's `judge_floor`, default 0.85), in `rule`
  mode, where the run may write;
- below the floor, `unsure`, `mode:recommend` or not routed, it only **records**
  the verdict (`attributes.judge`) and the proposal waits for a person;
- it never touches a statement an operator ruled on, and judges a statement once;
- every ruling of the run is **one ruling**: `revert_ruling` undoes the run;
- a random share of each run's acceptances (`judge_audit`, default 0.1, at least
  one) is marked `audited: pending` for a person to confirm (`accept_statement`)
  or overturn (`reject_statement`). List them with `status: asserted` and
  `judge_audit: pending` in the review queue.

Input (the run prompt, a JSON object, parsed strictly):

| field | default | |
|---|---|---|
| `scope` | `user` | `user`, `agent` or `tenant` (tenant needs the grants under [Scope](#scope), on `kb/judge`) |
| `write` | `rule` | `rule`, `recommend` (record verdicts, rule nothing) or `report` (no write at all; `would` counts the verdicts) |
| `max_judgements` | 50 | proposals considered (1–1000) |
| `max_verdict_calls` | 50 | model calls (0–1000); reaching either bound ends the pass `budget_stopped`, its verdicts recorded |
| `filter` | — | narrows the routed queue: `kind`, `method_prefix`, `document_id`, `from_document_id`, `to_document_id`, `under_path`, `from_under_path`, `to_under_path`; `routed: false` widens it to proposals the policy did not route, which are only ever recorded as recommendations |

The report carries ids and counts only: `considered`, `verdict_calls`,
`parse_errors`, `skipped`, then `judged`, `ruled`, `accepted`, `rejected`,
`escalated` (below the floor, unsure, not permitted), `recommended`, `refused`
(by code), `audit_marked` with `audit_ids`, `escalated_ids` and `ruling_id`.

Run a `report` pass first on a new deployment, as for the linker. Then schedule
it after the linker's drains, so what a drain proposed in the night is judged
before the morning's review:

```yaml
agents:
  kb/judge:
    memory_scopes: [tenant]           # a tenant pass, as for the linker
    sql_scopes: [tenant]
scheduled_runs:
  kb-judge:
    agent: kb/judge
    schedule: "0 5 * * *"             # daily, after the 03:00 ingest drain
    concurrency_policy: forbid        # one judge run at a time
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"scope":"tenant","write":"rule","max_judgements":200,"max_verdict_calls":200}' } ]
```

Or as a runtime schedule, over MCP `scheduledef` (or `POST /v1/_scheduledef`):

```json
{"op": "create", "name": "kb-judge",
 "overlay": {"agent": "kb/judge", "schedule": "0 5 * * *", "concurrency_policy": "forbid", "enabled": true,
   "prompt": [{"role": "user", "content": [{"type": "trusted-text",
     "text": "{\"scope\":\"tenant\",\"write\":\"rule\",\"max_judgements\":200,\"max_verdict_calls\":200}"}]}]}}
```

## Annotations

An annotation is two short texts about one part of a document: a **brief**
(one sentence saying what it is, so a reader can choose it) and an
**abstract** (one paragraph saying what it states, so a reader need not open
it). Agents get them with `with_summary: true` on the Document reads, and
people see them in the explorer and the chat link preview. `kb/annotator`
writes them; `kb/annotate` is its one model call.

**Bottom up.** For each document the annotator writes its sections from the
deepest up, then the document. A section is annotated when it has parts of its
own or its text is at least `min_body_chars` long (default 1500); a shorter
part is covered by its parent, whose request carries the part's text. Each
request carries the part's own text and its parts' briefs and abstracts —
never deeper — so it stays small whatever the size of the document. Every
request carries the document's identity (its code or key, as its class
declares it), and a section's names its document, so a section that names no
code of its own is never summarised under another; the prompt allows a code
only where the request carries it, and `set_annotation` rewrites a code of the
class's shape that is not the document's own. A document's request also
carries its relations to other documents, by kind and by what each reaches:
the title of another document, or the subject or topic itself for a link into
the tenant ontology, and its stored status (`STATUS: done`), which the brief
and abstract follow over the class description: a class described as a
proposal no longer makes a shipped document's brief call it one. The status is
not part of the inputs hash, so changing it alone does not make the annotation
stale; a pass with `force` rewrites it.

**Prompt version 2, per level.** The annotator keeps one prompt version per
level (`branch`, `document`, `path`), all 2 today. Raising one level's version
makes that level's annotations stale once and leaves the other levels current:
the next pass rewrites them (about one model call per annotated part). A raised
`document` version is found by every mode, a raised `path` version by `path`
and `sweep`, and a raised `branch` version only by a `document` pass: `drain`
and `sweep` pick a document by its document annotation. Estimate it with `write: "report"` first. A tenant that
forked `kb/annotator` or `kb/annotate` keeps the old code and prompt until it
re-forks them from this bundle.

Then the **folders** of the documents it looked at, from the deepest up to `/`.
A folder's request names each direct child's brief (documents and sub-folders,
the first 60, the rest grouped by class), the classes and topics of the
documents it covers and the kinds of relation among them. Its annotation is
kept on a document `<folder>/_annotation` of the operational type
`Annotation`, made on the first write; a folder that is itself a document
keeps it on that document's root. A folder brief that only repeats the
folder's name, or is under 20 characters, is replaced on write by the first
sentence of its abstract. A folder's annotation is withheld from a
caller who may not read every document it covers. The `/facts` and `/memory`
trees are never covered.

**Only what changed.** The runtime computes what a part is written from and
the hash of it (`annotation_inputs`), and refuses a write whose text moved in
between. A part whose hash is unchanged costs no model call: a changed passage
costs its own section, the sections above it and the document, nothing else.
An operator's hand-written annotation (`set_annotation` with `manual: true`)
is never rewritten.

**The ontology's say.** A class may name the slots its documents' annotations
fill (`@annotate the problem; the decisions taken; the phases and their
status`); the first prose line of the class's entry is shown with them as what
such a document is. The field `annotation_language` on the ontology root
enforces one language for every annotation.

**Modes.**

| `mode` | over | does |
|---|---|---|
| `document` | `document_id` or `path` | one document, then its folders |
| `path` | `path`, `recursive` | one folder; with `recursive: true` the folders beneath it first |
| `drain` | the `documents/ingested` queue | the documents it names, newest first, up to `max_messages` that need work, then their folders. It reads the queue from the oldest retained message and **never acknowledges it**: the queue's position is shared by its readers in the scope and belongs to `kb/linker`. A document that is current costs two reads |
| `sweep` | the scope, or `under_path` | every document that needs work, never-annotated first, then the least recently written, up to 1000 documents per run; then every folder beneath, deepest first |

| `write` | does |
|---|---|
| `report` (default) | counts what would be written per level, the model calls and the prompt characters they would send (`calls.estimated_chars`), and writes nothing. **Run this first**, and on an API-paid deployment before every schedule change |
| `annotate` | holds the scope's `annotator` lease, writes through `set_annotation`, releases the lease on every exit path |

Other input: `min_body_chars`, `max_body_chars` (the part's own text a
request carries, default 6000), `max_relations` (default 20), `max_calls`
(model calls per run, default 200), `max_writes` (default 500), `force`
(rewrite current annotations too, e.g. after a model change: the document's
parts in document mode, the folders in path mode; a forced document pass
leaves its folders to their hashes). Operational
documents (`SourceStub`, `SyncReport`, `Annotation`) and the `/facts` and
`/memory` trees are skipped.

**Report.** `{status, mode, write, prompt_versions: {branch, document, path}, documents, document_ids,
folders, stale: {branch, document, path}, calls: {made, estimated_chars}, skipped: {current,
uncovered_leaf, manual, operational, unreadable}, queue? | sweep?}`, plus
`would_call` / `would_write` in report mode, or `writes`, `parse_errors`,
`wrong_language`, `model_failures`, `refusals` and `lease` in annotate mode.
Ids and counts only, never store text. `status` is `complete`, `busy` (another
run holds the lease), `budget_stopped` (`max_calls` or `max_writes` reached),
`provider_unavailable` (three model calls failed in a row: the pass stops
rather than spend its budget on a provider that is down; a single failure
leaves its part stale and the pass goes on), `lease_lost` or `failed`.

**Grants and a schedule.** Annotations are kept for the tenant scope only, so
the annotator ships with both tenant grants (`memory_scopes: [tenant]`,
`sql_scopes: [tenant]`); you add nothing for them in your own layer. Its
writes stay those of the `annotate` grant above. Add the ingest channel and a
schedule:

```yaml
channels:
  documents/ingested: {scope: tenant}   # where tenant documents announce themselves
scheduled_runs:
  kb-annotator-ingest:
    agent: kb/annotator
    schedule: "30 3 * * *"            # after the linker's ingest drain
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","write":"annotate","max_messages":50}' } ]
  kb-annotator-sweep:
    agent: kb/annotator
    schedule: "0 4 * * 0"             # weekly: anything the drain missed
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"sweep","write":"annotate","max_calls":1000}' } ]
```

## Taxonomies

A taxonomy is a concept scheme of the tenant ontology that arranges documents
by topic. `kb/taxonomist` proposes one from the corpus: it reads the
**document briefs** (`list_annotations` level `document`, with each document's
title and type from `documents_summary`), never a document's text, so it needs
the annotations `kb/annotator` writes. `kb/taxonomy-writer` is its one model
call. It is the only agent of this bundle that proposes concepts; it files
nothing else and changes nothing in force.

**Modes.**

| `mode` | model calls | does |
|---|---|---|
| `report` (default) | none | reads the briefs and the ontology and counts what a pass would send: `batches`, `runs_used`, `calls.planned_domains`, `calls.planned_generate`, `calls.estimated_chars` (the characters a `generate` pass would send, an upper bound: every scheme in an update, the align request or the review is counted at `max_concepts` concepts of 400 characters) and `requests` (the largest single request, against the writer's limit). **Run this first** |
| `domains` | 1 | one request over a spread sample of `batch_size` briefs: three to five taxonomies worth building, each with a `name`, a one-line `domain`, the declared classes it would arrange (`projects`; a class the ontology does not declare is dropped and counted) and `why`. One already in force is marked `exists`. **Reported only**: an operator picks which to generate |
| `generate` | `runs_used` x batches + 1 (review) + 1 (align, when `runs_used` is 2 or more; not made when fewer than two runs produce a scheme) | builds the scheme named by `scheme`, for the one-line `domain`. The briefs are cut into batches of about `batch_size`. Each run asks a generate request over its first batch and an update request per further batch, run `r` starting at batch `r`, so each run meets the corpus in a different order. `runs` is clamped to the number of batches (`runs_used`, `runs_clamped`): a further run would meet the same briefs in the same order as an earlier one, and agreeing with it would show nothing. With two runs or more, an align request groups the runs' concepts that mean the same topic, however worded; the code checks the groups and keeps those a majority of the runs hold (see **Recurrence** below; with one run (`runs: 1`, or a corpus of one batch) all of them, and no align request), and a review request names each kept concept by one of its wordings, merges overlapping siblings and checks the shape; whatever the review adds is dropped. With `write: "propose"` (the default) the result is filed through `propose_entity` as **one whole-scheme proposal** with `@origin generated`, `@domain` and `@projects`: inert until an operator accepts or rejects it in the Ontology page. `write: "none"` reports the outline and files nothing |

One generation is not a taxonomy: measured on one corpus, five generations
agreed only partly on the top-level split (pairwise adjusted Rand index 0.44
to 0.59). That is why `generate` runs up to three times by default, one run
per batch, and keeps what recurs. A corpus of one batch (up to `batch_size`
briefs) gets one generation.

**Recurrence.** Which concepts recur is counted in code, not asked of the
model: asked to keep what recurs, the review unioned the runs (on one corpus,
runs of 66 and 91 concepts came back as 102, overlaps kept). Matching by
wording alone is too literal the other way: on the same corpus two runs of 110
and 105 concepts kept 21, losing central topics both runs found under other
words (`runs and agents` and `agent runs and tools`; `deployment` and
`configuration` against `deployment and configuration`; `documents` and
`search and retrieval`). So with two runs or more one **align** request shows
the writer every run's concepts, each with an id (`r1.c7`: run 1, its seventh
concept), its name, meaning and parent's id, as untrusted data, and asks for
groups of concepts from different runs that mean one topic
(`{"groups": [{"name", "member_ids", "parent_group"}]}`), including one run's
split siblings against another's single concept. The reply is a proposal,
checked in code: a member must be an id the request held (`unknown_id`) and is
used once in the whole reply (`reused_id`); two members of one run must be
siblings the group merges (`not_siblings`); a group's name must be one of its
members' names, or the most common member name is used (`invented_name`), so
the model cannot add a topic; an entry with no `member_ids` list is
`malformed`, one left with no member `empty`. A group counts the distinct
runs among its members and is kept when that is a majority (`below_need`
otherwise: a group of one run's concepts never recurs). Every concept no kept
group holds is then matched by its own key as below, so concepts the runs
worded alike stay matched though the align reply left them out; one whose
key is already a wording of a kept group joins that group (and adds its
runs) rather than standing beside it under the same wording. When the
align call fails, its reply holds no `groups` list, or its request would not
fit the writer's window (it is measured before it is sent, not refused), the
pass falls back to matching every concept by key and reports `align_failed`.
A concept is matched by key across runs wherever a run puts it: the name's tag
segment, each word without its punctuation and made singular, `and`, `of`,
`the` and the like left out and the rest sorted (`Memory recall` and
`memory-recall`, `hooks and lifecycle events` and `lifecycle events and
hooks`, `search (web)` and `web search`, `movies` and `movie` are one; `news`
and `new`, `notes` and `not` are not). Only plain ASCII words are made
singular; a word in another script (Cyrillic, say) matches when it is
written the same, case aside. A concept is kept when a **majority** of the
runs that produced a scheme hold it: two of two, two of three, three of four
or five. "At least half" would keep one run of two, the union again. A run
that produced no scheme lowers the majority (two of three runs left need
two; one left keeps everything, with no align request), and `runs_failed`
says so. The kept tree puts an aligned group under its `parent_group` when
that group is kept (else under the nearest kept group up the reply's
`parent_group` chain), and otherwise each concept under its nearest kept
ancestor in the first run that names it (its parent when kept, else the next
one up, else the top level), never deeper than `depth`. The review is shown
only the kept set (`KEEP`, each concept with every wording the runs used) and
its reply is held to it before any cap is applied: a concept named by any
wording of a kept concept is that concept, under whichever of its names the
review chose; one whose key no kept concept holds is dropped and counted (its
kept children take its place); each kept concept is kept once. So the
scheme filed is never larger than the kept set, and a concept the review
added cannot push a kept one past the sibling cap. The report's `recurrence`
gives `need` (runs a concept must be in), `runs_with_scheme`, `runs_failed`,
`per_run` (`[{run, concepts}]` for each run that produced a scheme), `kept`,
`not_recurring` (concepts matched by key that too few runs hold),
`added_by_review`, `kept_missing` (kept concepts the review's scheme left
out, for example by merging two under a new name), `align` (`done`,
`failed`, `too_large` or `one_run`), `align_failed`, `aligned_groups` (groups
that passed the checks), `kept_groups` (of those, the ones kept),
`kept_by_key` (concepts kept by key matching), `merged_into_aligned` (key
matches that joined a kept group of the same wording) and
`dropped_by_check` (the counts above).

Other input: `projects` (class names: only documents of those classes and
their subclasses are read, and the scheme's `@projects` is set to them; an
undeclared class stops the run with `unknown_class` before any call),
`under_path` (only documents under this folder), `batch_size` (default 200,
20 to 400), `max_briefs` (default 1000, at most 5000: a larger corpus is
sampled evenly in chunk-id order, which is random and stable), `runs`
(default 3, 1 to 5, at most the number of batches), `depth` (1: top-level
concepts only, 2: and their children; default 2), `max_concepts` (default
150), `max_calls` (default 30). Operational documents (`SourceStub`,
`SyncReport`, `Annotation`) and the ontology document are skipped.

**What is enforced in code.** Every brief and title goes to the writer as an
untrusted data block, never in its trusted prompt, and so does every scheme a
model wrote. A reply is data: a concept is kept only with a one-line name of at
most 80 characters that `propose_entity` would take (not starting with `@` or
`#`, with a tag segment, and with a letter or digit), a meaning and an excludes
line within their lengths, at most 25 siblings and `max_concepts` in all, no
two names with one tag segment (`machine learning`, `machine-learning` and
`Machine/Learning` are one) and no deeper than `depth`; anything else is
dropped and counted in `dropped`. A concept's excludes line is filed in its
meaning (`... Excludes: ...`). Before its first model call a `generate` pass
stops with `budget_refused` when its planned calls exceed `max_calls`, with
`input_too_large` when its largest request at worst (a review of every run's
scheme at `max_concepts`) would not fit the writer's window less its output
cap (`domains` checks its one request the same way), with `scheme_exists`
when a scheme in force has the name's tag segment, and with `scheme_proposed`
(and `proposal_status`) when a whole-scheme proposal of that name is already
pending or was rejected: an operator has seen it. Three failed model calls in
a row stop it with `provider_unavailable`. The report carries ids, counts, the
names the model proposed and its token usage, never a brief.

**Report.** `{status, mode, ontology: {exists, confirmed, classes, schemes},
corpus: {annotations_read, scan_truncated, documents, used, sampled, skipped},
batches, runs_used, runs_clamped?, requests: {largest_chars, domains_chars,
limit_chars}, calls: {planned_domains, planned_generate, made,
estimated_chars}}`, plus in `domains` and `generate`: `model_failures`,
`parse_errors`, `cut` (replies stopped at the output cap), `dropped`, `usage`
(tokens summed over the writer's runs), `models`, `runs` (the writer's run
ids), and `domains`, or `candidates` (per run: its concepts and top-level
names), `recurrence`, `scheme` and `proposal: {filed, chunk_id, concepts}` (or `refusal`).
`status` is `complete`, `refused`, `unreadable` (a read failed, e.g. no tenant
grant), `empty` (no briefs), `no_ontology`, `unknown_class`, `scheme_exists`,
`scheme_proposed`, `budget_refused`, `input_too_large`, `budget_stopped`,
`provider_unavailable`, `domains_failed`, `generate_failed` (no run produced a
scheme), `nothing_recurs` (no concept is held by a majority of the runs; no review is asked), `review_failed`, `filing_refused` or `failed`. `proposal_check:
partial` says the ontology held more proposed or rejected chunks than one read
returns (1000), so a proposal of the same name may not have been found; filing
would then be refused at the end.

**Grants.** The briefs are in the tenant scope, so the taxonomist ships with
both tenant grants (`memory_scopes: [tenant]`, `sql_scopes: [tenant]`); as for
the annotator, you add nothing in your own layer. It holds `Document` in full
mode only because `propose_entity` is a write op; its code calls
`describe_ontology`, `list_annotations`, `documents_summary`, `query_chunks`
and `get_chunk` (to find a scheme proposal of the same name) and
`propose_entity`, and nothing else, and filing a proposal needs no statement
method. `kb/taxonomy-writer` holds no grant at all.

```json
{"mode": "report"}
{"mode": "domains"}
{"mode": "generate", "scheme": "subjects", "domain": "What the work is about, for finding the documents on one subject.", "write": "none"}
{"mode": "generate", "scheme": "subjects", "domain": "What the work is about, for finding the documents on one subject."}
```

## Building the ontology

`kb/ontologist` reads what the tenant's corpus already uses and the ontology
does not declare (`Document` op `ontology_signals`): the types on documents,
chunks and facts that no class declares, and the relation kinds no relation
declares, counted over every scope of the tenant. Its report says which of
them a propose pass would suggest as new classes and relation kinds. This
release runs **report mode only**: it calls no model and writes nothing.
Propose mode (each candidate judged twice by `kb/ontology-writer`, filed with
`propose_entity` only when both answers agree) follows a measurement of the
report on a real tenant.

**What it drops**, each counted under `dropped`:

| reason | a candidate that |
|---|---|
| `rejected` | an operator rejected a proposal of the same name |
| `proposed` | already has an open proposal of the same name |
| `below_floor` | a type on fewer than 3 rows or in fewer than 2 documents; a kind with fewer than 3 statements that are neither rejected nor expired |
| `bad_name` | is not one line of at most 80 characters |
| `cap` | is past the ten largest (at most ten proposals a pass) |

Types the runtime owns (`root`, `section`, `image`, `mermaid`, `canvas`) and
the operational document types (`SourceStub`, `SyncReport`, `Annotation`) are
left out by `ontology_signals` itself, as are declared, retired and renamed
names. A type's populations (document, chunk, fact) count as one candidate;
its `documents` is the largest population's count, a lower bound.

**What it reports**, per candidate: `section` (`classes` or `relations`), the
stored `name`, the `signal`, the counts (`rows` and `documents` for a type;
`statements` and `live` for a kind; `tenant_scope` and `other_scopes`), and
the `directives` the code would attach. A relation gets a `domain` and a
`range` only when one pair of end types holds at least 80% of its statements
and both are classes in force; otherwise none, with `directives_note`
`no_dominant_pair` or `ends_not_declared`. The 80% is a starting value, not a
measured one. A class gets no directive. The model never chooses a name or a
directive.

The report also carries `backlog.open`, the open proposals `kb/ontologist`
filed (a propose pass stops at 30), `next_pass` (`propose`, `backlog` or
`nothing`: what a propose pass would do now), `signals` (whether every scope
was counted, the lists cut at their limit, and the statements of declared
kinds whose ends the kind does not admit, which this agent does not propose),
and no title, body or quote.

**Grants.** The signals count every user's scope of the tenant, so the runtime
answers them only to a code agent granted `statement_methods: [ontology]`
that holds both tenant scopes and whose access-control rules hide no tenant
document; otherwise the report is `status: unreadable` with the refusal. The
bundle ships these grants. `Document` is held in read mode: its code calls
`describe_ontology`, `ontology_signals`, `query_chunks` and `get_chunk` (to
count the backlog), and the runtime refuses any write op.
`kb/ontology-writer` holds no grant at all.

```json
{"mode": "report"}
```

Nothing is scheduled. Run it by hand, read the report, and propose what you
agree with yourself (`propose_entity`, or the Ontology page) until propose
mode ships.

## Scope

`scope: user` works with the bundle as shipped. A tenant pass (which L3 needs
in order to write) requires both tenant grants on the linker. The bundle leaves
them out on purpose, because a tenant Document grant is a tenant write grant:

```yaml
agents:
  kb/linker:
    memory_scopes: [tenant]
    sql_scopes: [tenant]
```

## Cost

The linker makes no model call except in L4, L5 and L6's `rejudge`. L4 spends one
`kb/verdict` call per claim-bearing passage that has candidates left after its
filters, capped by `max_verdict_calls` and `time_budget_seconds`, and runs only
when asked for; L5 spends one per passage
that has norms, and `rejudge` one per changed statement, each under the same
cap. `kb/judge` spends one `kb/judge-verdict` call (tier `middle`) per proposal
it judges, capped by `max_verdict_calls`. `kb/annotator` spends one
`kb/annotate` call (tier `middle`) per section, document or folder whose inputs
changed, capped by `max_calls`; a first pass over a corpus is about one call per
annotated section, plus one per document and one per folder, and
`write: report` prints the estimate (`calls.estimated_chars`) before you spend
it. `kb/taxonomist` spends one `kb/taxonomy-writer`
call (tier `middle`) per request: one in `domains`, `runs_used` x batches + 1
(review) + 1 (align, when `runs_used` is 2 or more) in `generate` (6 for 379
briefs at the defaults: two batches, so two runs), capped by `max_calls`; `report`
prints the plan first. `kb/ontologist` makes no model call in this release.
Nothing is scheduled unless you add a schedule.
