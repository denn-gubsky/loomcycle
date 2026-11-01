---
name: Document/describe_ontology
description: "Document op=describe_ontology — the tenant ontology in force, parsed: classes with their effective directives and fields, relation kinds, grammars, concept schemes and instances."
---
`describe_ontology` returns the tenant's ontology as data. It is for code that has
to act on the ontology, and it saves that code from parsing the document itself.
It reads the tenant's ontology whatever `scope` the call names, and takes no other
arguments.

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

## Returns

`{exists, document_id?, status, confirmed, version, sections, partial?, classes,
relations, grammars, schemes, instances, review?, issues?}`:

- `exists` — false when the tenant has no ontology document, so only the standard
  classes apply.
- `status` — `draft` or `confirmed` (`""` without a document). Sections have no
  status of their own.
- `version` — the ontology's version: the one stamped on facts and relations
  written under this vocabulary.
- `sections` — the declared section kinds, in document order.
- `partial` — part of the document could not be read. Do not treat a missing
  entry as removed; read again.
- `classes` — each `{name, parent?, source, kind, fields?, title_identity?,
  directives?, memory_scope?, renamed_from?, issues?}`.
  - `kind` is `document` for a document class, `chunk` otherwise.
  - `fields` is the complete typed field set, inherited fields first.
  - `directives` holds `identifier_pattern`, `title_pattern`, `anchor_grammar`,
    `citation_grammar`, `definitions` and `exclude`, each as declared or else
    inherited.
  - `definitions` is a pattern (RE2 syntax) over chunk titles. In a document of
    the class, a chunk whose title matches it, and every chunk under that one, is
    where the document defines its terms rather than uses them. `get_chunk` and
    `query_chunks` mark such chunks `definitional: true`, so you need not run
    the pattern yourself (the root chunk, the document's own title, is never
    marked).
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
- `schemes` — each concept scheme with its concepts: `name`, `path`, `parent`,
  `tag`, `notation`, `alt`, `rank`, `exact_match`, `retention`, `acl`.
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
  `rule:matched` (a rule made it, not a model), `confidence<x`, `<=x`, `>x`,
  `>=x` (the writer's own confidence; a statement with none meets no bound).
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
{"exists": true, "document_id": "0b6f1e2a9c4d4e7f8a1b2c3d4e5f6a7b", "status": "confirmed", "confirmed": true, "version": 14, "sections": ["classes", "relations", "instances"], "classes": [{"name": "Law", "parent": "Document", "source": "tenant", "kind": "document", "fields": [{"name": "title", "type": "text", "required": true}], "directives": {"anchor_grammar": "law-part", "citation_grammar": "law-ua", "definitions": "(?i)визначення термінів"}}], "relations": [{"name": "amends", "inverse": "amended_by", "domain": ["Law"], "range": ["Law"], "dated": true, "source": "tenant"}], "grammars": [{"name": "law-ua", "class": "Law", "patterns": ["Закон(?:у)? України № ?(?P<number>\\d+-[IVX]+)"], "resolve": "work_key=law:{number}", "used_as": ["citation"]}], "schemes": [], "instances": [{"name": "Acme Bank", "class": "organization", "alt": ["Acme"], "chunk_id": "5c2d8e1f0a3b4c6d9e7f1a2b3c4d5e6f", "registry_key": "organization:acme-bank", "subject_chunk_id": "9a8b7c6d5e4f40312a1b0c9d8e7f6a5b"}]}
```
