---
name: Document/describe_ontology
description: "Document op=describe_ontology — the tenant ontology in force, parsed: classes with their effective directives and fields, relation kinds, grammars, concept schemes and instances."
---
`describe_ontology` returns the tenant's ontology as data. It is for code that has
to act on the ontology, and it saves that code from parsing the document itself.
It reads the tenant's ontology whatever `scope` the call names. Its other
arguments, `with_counts` and `with_stats`, add how many documents each concept
and class has, and the node statistics `node_stats` stored for them.

It describes what is **in force**, the way the runtime applies it. A draft ontology
governs nothing, so it describes the standard classes and nothing else, and it
lists relation kinds only when a confirmed ontology has a Relations section and
instances only when it has an Instances section.

Read it **first** when you are about to write into a knowledge base you do not
know, and use what it names rather than inventing your own:

- `classes` — the `type` to give a document's root or a chunk, and, with
  `subject`, the entity types `upsert_chunk` accepts. A class's `fields` are the
  ones to fill; its `@identity` fields build the `natural_key` for you.
- `relations` — the `kind` to pass to `link_chunks` and `propose_statement`,
  with the classes each end must be (`domain`, `range`), its `qualifiers` (the
  `attributes` it accepts) and whether it is `dated` (give it `valid_at`).
  `walk: true` kinds are the ones `graph_recall` and `search` `expand` follow by
  default.
- `schemes` — the concepts to filter `search` and `related` by (`concept`,
  `concept_prefix`, by `tag`), and, in the tenant scope, the targets of a
  `subject` relation.
- `instances` — the real organisations, units and posts the tenant knows. Write
  a statement about one at its `subject_chunk_id`; never create a second
  subject for it.

Missing something? Suggest it with `propose_entity` (a class, a relation kind
with `section: relations`, a concept with `section: schemes`, an instance with
`section: instances`). An agent never edits the ontology document itself.

## Arguments

- `scope` — ignored: the ontology is always the tenant's.
- `with_counts` — `true` adds member counts (see Member counts below). Off by
  default; without it the answer is exactly what it is without the argument.
- `with_stats` — `true` adds the member counts and the stored node statistics
  (see Node statistics below). Off by default, with the same guarantee.

## Returns

`{exists, document_id?, status, confirmed, version, content_version?, sections,
partial?, classes, relations, grammars, schemes, instances, norm_classes, policy,
review?, issues?}`:

- `exists` — false when the tenant has no ontology document, so only the standard
  classes apply.
- `status` — `draft` or `confirmed` (`""` without a document). Sections have no
  status of their own.
- `version` — the ontology's version: the one stamped on facts and relations
  written under this vocabulary.
- `content_version` — 16 hex characters naming what the ontology's entries
  declare. It changes when a class, relation kind, scheme, concept, instance or
  grammar is added, changed (its meaning included), retired, moved under another
  parent or removed, and returns to its old value when the change is undone. It
  does not change on a proposal or a rejection, an edit of the Review section,
  anything under a section this version does not know, or a reorder of siblings
  (a concept's place in an `@ordered` scheme is its rank, so that does count).
  Use it to tell whether the vocabulary moved between two reads; `version` also
  moves on a confirm. Absent without a document and when `partial` is true.
- `sections` — the declared section kinds, in document order. A heading whose
  first directive is `@section` with a kind this version does not know is not
  listed, nothing under it is in force (no class, relation, concept or
  instance), and `issues` names it.
- `partial` — part of the document could not be read. Do not treat a missing
  entry as removed; read again.
- `classes` — each `{name, parent?, source, kind, meaning?, fields?,
  title_identity?, norm?, inherited_norm?, directives?, memory_scope?,
  renamed_from?, alt?, issues?}`.
  - `meaning` is the first prose line of the class's entry: what a thing of the
    class is.
  - `kind` is `document` for a document class, `chunk` otherwise.
  - `alt` is the class's own other names, one `@alt` line each, as on a
    concept: what people call a document of the class ("playbook" for a
    `Runbook`). Not inherited.
  - `renamed_from` is the name the class had before (`@renamed_from`). It reads
    as the class: a `type` filter for either name finds the rows stored under
    both, and a row still typed with the old name is declared, counted and
    checked as one of the class. It is not honoured, and `issues` says so,
    when two classes claim the name (it then reads as neither) or when it is
    the name of a class in force (which keeps it).
  - `fields` is the complete typed field set, inherited fields first.
  - `directives` holds `identifier_pattern`, `title_pattern`, `anchor_grammar`,
    `citation_grammar`, `definitions`, `boilerplate`, `exclude` and `annotate`,
    each as declared or else inherited. `annotate` is the slots an annotation of
    a document of the class fills, as one `a; b; c` list.
  - `definitions` is a pattern (RE2 syntax) over chunk titles. In a document of
    the class, a chunk whose title matches it, and every chunk under that one, is
    where the document defines its terms rather than uses them. `get_chunk` and
    `query_chunks` mark such chunks `definitional: true`, so you need not run
    the pattern yourself (the root chunk, the document's own title, is never
    marked).
- `norm` — the class is a norm: a document class the ontology marks `@norm`, or a
  subclass of one (then `inherited_norm` is true too). A norm's documents are the
  rules other documents are measured against.
- `norm_classes` — every norm class in force, sorted; `[]` when the ontology marks
  none (a draft marks none). Compare a document against these classes' documents
  when the caller names no target types of its own.
- `relations` — each relation kind in force, with its `source` (`seed`,
  `reserved` or `tenant`), `parent`, `inverse`, `domain`, `range`, its
  characteristics, `qualifiers`, `discriminator`, `dated`, `walk`, `auto_assert`
  and `renamed_from`.
- `grammars` — each `{name, class?, patterns, resolve, scopes?, anchors?, issue?,
  used_as?}`.
  - `scopes` and `anchors` are its `scope:` and `anchor:` lines, each
    `{pattern, templates}` (see Grammar lines below).
  - A grammar with an `issue` is disabled and has no patterns, scopes or anchors.
  - `used_as` says how classes name it: `anchor`, `citation` or both. A grammar
    with no `pattern:` line finds no citations; named as a citation grammar it is
    reported as an issue.
- `schemes` — each concept scheme with its header, when it declares one
  (`domain`, `projects`, `origin`, `source`, `version`; see Scheme header below),
  and its concepts: `name`, `path`, `parent`, `tag`, `notation`, `alt`, `rank`,
  `exact_match`, `focus`, `retention`, `acl`, and the concept's effective policy:
  - `effective_retention` — its own `@retention`, else the nearest ancestor's
    (then `inherited_retention` is true and `retention_from` is that ancestor's
    tag).
  - `effective_groups` — every `@acl` group from the top of the scheme down to
    the concept: a narrower concept only adds groups. A group the tenant does
    not have is left out and reported.
- `policy` — `{enforced, note, open_mode?, issues?}`. `enforced` is false: the
  policy is reported, nothing removes access or deletes by age because of it
  yet. `open_mode` is true when no access-control rules apply to the caller.
  `issues` lists what contributes nothing (an unknown group, a period that does
  not parse, a `@retention` or `@acl` on a concept still proposed) and a warning
  where no member of the tenant is in every group a concept requires (checked
  only for a caller who may see those groups' members). A draft sets no policy.
  Use `effective_policy` for one document's policy.
- `instances` — each real organisation, unit or post the Instances section
  declares, in document order: `{name, class?, alt?, chunk_id, registry_key?,
  subject_chunk_id?, issues?}`.
  - `class` is the instance's `@a` class; an instance without one, or whose class
    is not declared, carries an issue and is never adopted.
  - `chunk_id` is the instance's chunk in the ontology document.
  - `registry_key` and `subject_chunk_id` name the subject node the instance was
    adopted as when the ontology was confirmed — the node to write statements at.
    They are absent until the instance is adopted, when its recorded key is not one
    adoption trusts, and when the caller may not read the subject (it needs the
    tenant scope and read on the subject's document). Absent does not prove the
    instance is unadopted; do not create a subject for it.
- `review` — the review policy in force: which proposed statements need no
  person. `{source, rules, judge_floor, judge_audit, applied, issues?}`.
  - `source` is `review` for the ontology's Review section, or `auto_assert`
    when there is none and relation kinds are marked `@auto_assert` (each such
    kind is then the rule `accept kind:<kind> method:rule:* when
    quotes:resolved`). Absent when neither applies.
  - `rules` in order, each `{index, text, action, any?, kinds?, method?,
    quotes_resolved?, self?, targets?, confidence?, rule_matched?, floor?,
    mode?}`. The first rule that matches a proposal decides it; a proposal no
    rule matches waits for a person.
  - `applied` is false when the section has issues: then no rule applies and
    every proposal waits for a person. The issues are also in `issues`.

## Member counts

With `with_counts: true` the answer also says how much of the tenant scope each
concept and class covers, counting only documents you may read:

- each concept gets `members` — the documents at the concept or under a
  narrower one, each counted once, however many of its chunks or narrower
  concepts reach it. It is exactly the set `search` with `concept:<tag>` in the
  tenant scope draws from: a document is a member when one of its chunks has a
  live `subject` statement (or one of a narrower kind) to the concept. Proposed,
  rejected, refuted and retired statements do not count.
- each concept gets `members_by_class` — the same documents split by their own
  class (a document with no class is in `members` only). Absent when no member
  has a class.
- each scheme gets `members` — the documents about any of its concepts, each once.
- each scheme gets `unplaced` — the documents of the classes the scheme
  projects (its `@projects` classes and their subclasses; without `@projects`,
  every document class) that are under none of its concepts by the same rule
  as `members`. A document with no class is never counted, and neither are the
  ontology document itself and the operational types `classify` skips
  (`SourceStub`, `SyncReport`, `Annotation`). Use it to see which taxonomy has
  documents waiting to be placed; `memberships` shows where one document sits.
- each class gets `documents` — the documents whose type (their root chunk's
  class) is that class and, in a confirmed ontology, one of its subclasses, as
  `search` with `type:` matches them. In a draft no subclass is included, since
  a draft widens nothing.

Counts are read on every call; they are never part of the ontology itself.
They need the tenant scope: a caller without it gets no counts and an issue
saying so. When the counts cannot be read, the answer is `partial` with an issue
and carries none.

## Node statistics

With `with_stats: true` the answer carries the counts above and, on each concept
and each document class whose statistics `node_stats` has stored, `stats`.

The statistics are computed only from the documents every member of the tenant
may read: a document the tenant's access-control rules keep from some member
(a deny bound to a group, a user, a token or the whole tenant) is left out of
them entirely — never counted, never an exemplar, never in the terms. They are
then the same for every reader of the ontology; nothing is withheld from a
reader with access-control limits. The counts above are not: they stay the
caller's own, the documents it may read. A member whose document annotation is
metadata-only (written from a catalogue record without its content) is counted
in `members` and left out of the centroid, the exemplars and the terms.

- `members` and `members_with_vector` — the members when they were computed,
  and how many of them had a document-level annotation vector;
- `spread` — the mean cosine distance of those vectors to their centroid (0:
  all alike; nearer 1: scattered);
- `exemplars` — the three members nearest the centroid, `{document_id, title}`;
- `siblings` — the names of the three nodes under the same parent nearest by
  centroid;
- `terms` — up to ten words frequent in the members' briefs and abstracts and
  rare in the siblings';
- `computed_at` (unix nanoseconds) and `stale` — true when the node's
  definition, its members, a member's annotation or the embed model changed
  since.

Without a vector store (Postgres with pgvector) a stored row has counts only.
A node with no stored row has no `stats`, and neither has one whose row was
stored before statistics left out restricted documents, until `node_stats`
computes it again. A stale row is shown, marked `stale`, while the node's
members are still the ones it was computed from. When they are not — a member
was restricted, filed under a denied name, turned metadata-only or added since —
`stats` holds only today's `members`, `computed_at`, `stale: true` and
`recomputing` (why), until the next `node_stats` pass: spread, terms,
exemplars and siblings drawn from the old members are not served. Vectors and
centroids are never returned.

Beside `stats`, a concept or document class with a stored series (see
`measure`) carries `series`: `{period, latest, count, share?, fallback_share,
trend}`. `latest` is the last period holding a member, with its `count`, a
concept's `share` of its scheme's things dated then, and `fallback_share`, the
share of those members dated only by when they were stored. `trend` is
`{previous, previous_count, previous_share?, change}`: the period before, and
the change of share since (of count where the share is not known). It is served
beside the node, never written into a summary, and follows the same rule as
`stats`: built from the documents every member may read, the same for every
reader.

## Review rules

The Review section is a root chunk whose first line is `@section review`. Each
list item in its body is one rule; a `judge_floor: <0..1>` line sets the
confidence a judge's verdict needs to rule (default 0.85), and a
`judge_audit: <0..1>` line the share of each judge run's acceptances marked
for a person's audit (default 0.1, at least one; 0 is refused). Prose is not
read.

```text
- accept kind:cites,repeals method:rule:citation when quotes:resolved self:false target:held|placeholder
- accept kind:subject method:classify when rule:matched
- reject kind:elaborates method:extractor:relate when confidence<0.6
- judge  kind:elaborates,uses_term floor:0.9 mode:recommend
- human  *
```

- Actions: `accept`, `reject`, `judge` (leave it for the judge agent), `human`
  (leave it for a person).
- Selectors: `kind:<a,b>` (exact kinds), `method:<m>` (exact, or a prefix
  ending in `*`, as `rule:*`), or `*` for every proposal.
- Conditions, after `when`: `quotes:resolved` (it has evidence, and every
  quote is in a chunk and not stale), `self:true|false` (both ends in one
  document), `target:held|placeholder|subject|concept` (what the to-end is),
  `rule:matched` (a rule made it: a `rule:` method, or classify's class rules
  or instance patterns — not the neighbours' vote, not a model),
  `confidence<x`, `<=x`, `>x`, `>=x` (the writer's own confidence; a
  statement with none meets no bound).
- `judge` options: `floor:<0..1>` and `mode:rule|recommend`.

## Grammar lines

A grammar is a `grammar:<name>` chunk in the classes section. Its body is lines:

- `pattern: <regex>` and `resolve: work_key=…, anchor=…, kind=…` — a citation
  grammar's matches, and the templates each fills (`{group}` is a named group of
  the pattern). As an anchor grammar, the deepest heading a pattern matches fills
  `anchor=`.
- `scope: <regex> => <alt> | <alt> …` — a heading it matches is a scope (the
  resolution around an act, an annex). Its segment prefixes every part inside it.
- `anchor: <regex> => <alt> | <alt> …` — keys a part by its own heading, or by
  the nearest heading above it within its scope.

Each alternative is a template; the first whose `{group}`s all took part in the
match is used, and one without groups always is. The split is on the last ` => `.
A part's anchor is its scope segments, outer to inner, then its leaf. A scope
heading's own part is keyed by the scopes alone. A part with no leaf inside its
innermost scope keeps its heading-path anchor. A grammar with `anchor:` lines
needs no `resolve:` line. Patterns are RE2: `\b` is an ASCII word boundary and
never matches after a Cyrillic letter, so end a word with `(?:\s|$)`.

A resolution approving a regulation that has annexes, where every scope numbers
its points from 1 (a point's heading quotes its text and ends with «…», a
chapter's is a short title):

```text
##### grammar:nbu-act
scope: ^Постанова$ => resolution
scope: ^Додаток(?:\s+(?P<n>\d+))?(?:\s|$) => annex-{n} | annex
anchor: ^(?P<n>\d+)\.\s.*…$ => point-{n}
anchor: ^(?P<n>\d+)\.\s => chapter-{n}
```

keys `Постанова › 1. Затвердити Положення…` as `resolution/point-1`, its
`Додаток до постанови` as `resolution/annex`, the regulation's chapter
`1. Основні положення та терміни` as `chapter-1` and its point
`1. Це Положення розроблено…` as `point-1`, `Додаток 2 до Положення` as `annex-2`
and the point 3 inside it as `annex-2/point-3`. The regulation's own heading is no
scope, so its parts carry no prefix.

## Scheme header

A concept scheme is a taxonomy: one knowledge domain's arrangement of the
things the classes say exist. Its entry may say what it is for and where it
came from, and a concept may name the thing it is about:

```text
### aircraft-systems
Maintenance topics, by system.
@domain Aircraft maintenance: which system a task or report concerns
@projects MaintenanceTask, Drawing
@origin adopted
@source https://example.org/ata-chapters
@version 2024
@period week
#### Landing gear
@focus LandingGear
```

- `@domain <text>` — the knowledge domain the scheme covers, one line.
- `@projects <Class>, <Class>` — the classes whose things it arranges
  (document, entity or fact classes; subclasses included). Without it, every
  document class.
- `@origin adopted|declared|generated` — a published scheme taken over, one
  the tenant wrote, or one generated from the corpus. Accepting a proposed
  adopted scheme writes `@locked` on each of its concepts.
- `@source <text or URL>` and `@version <text>` — an adopted scheme's
  provenance.
- `@period day|week|month` — the period the scheme's series are kept in (see
  `measure`); month without it. Weeks are ISO weeks, in UTC.
- `@focus <class or instance>` on a concept — the entity the concept is about.
  Confirming the ontology writes it as a `focus` statement from the concept's
  chunk to the class's entry, or to the subject an instance was adopted as;
  removing or changing it retires that statement.

A `@projects` class that does not exist, an `@origin` or `@period` outside
the three, and a `@focus` that names no class or instance are reported in the
entry's `issues` and ignored.

## Errors

- A permission error when the caller's access-control rules do not let it read
  the tenant's ontology document.
- `describe_ontology: the tenant ontology could not be read` is a storage
  failure; retrying is reasonable.

## Examples

```json
{"op": "describe_ontology"}
```

```json result
{"exists": true, "document_id": "0b6f1e2a9c4d4e7f8a1b2c3d4e5f6a7b", "status": "confirmed", "confirmed": true, "version": 14, "content_version": "5f1c0a9e3b7d2468", "sections": ["classes", "relations", "instances"], "classes": [{"name": "Law", "parent": "Document", "source": "tenant", "kind": "document", "fields": [{"name": "title", "type": "text", "required": true}], "directives": {"anchor_grammar": "law-part", "citation_grammar": "law-ua", "definitions": "(?i)визначення термінів"}}], "relations": [{"name": "amends", "inverse": "amended_by", "domain": ["Law"], "range": ["Law"], "dated": true, "source": "tenant"}], "grammars": [{"name": "law-ua", "class": "Law", "patterns": ["Закон(?:у)? України № ?(?P<number>\\d+-[IVX]+)"], "resolve": "work_key=law:{number}", "used_as": ["citation"]}], "schemes": [], "instances": [{"name": "Acme Bank", "class": "organization", "alt": ["Acme"], "chunk_id": "5c2d8e1f0a3b4c6d9e7f1a2b3c4d5e6f", "registry_key": "organization:acme-bank", "subject_chunk_id": "9a8b7c6d5e4f40312a1b0c9d8e7f6a5b"}]}
```

With member counts (a confirmed ontology with a `topics` scheme; other fields cut
for brevity):

```json
{"op": "describe_ontology", "with_counts": true}
```

```json result
{"exists": true, "status": "confirmed", "confirmed": true, "version": 3, "sections": ["classes", "schemes"], "classes": [{"name": "Provision", "parent": "Document", "source": "tenant", "kind": "document", "documents": 12}], "schemes": [{"name": "topics", "chunk_id": "3e1d0c9b8a7f46e5d4c3b2a190f8e7d6", "members": 9, "concepts": [{"scheme": "topics", "name": "Risk Management", "path": ["Risk Management"], "tag": "topics/risk-management", "chunk_id": "7a6b5c4d3e2f41a0b9c8d7e6f5a4b3c2", "members": 7, "members_by_class": {"Provision": 6}}, {"scheme": "topics", "name": "outsourcing", "path": ["Risk Management", "outsourcing"], "parent": "Risk Management", "tag": "topics/risk-management/outsourcing", "chunk_id": "1f2e3d4c5b6a47980a1b2c3d4e5f6a7b", "members": 3, "members_by_class": {"Provision": 3}}]}], "instances": [], "norm_classes": []}
```
