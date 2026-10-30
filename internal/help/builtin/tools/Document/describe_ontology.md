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

## Arguments

- `scope` — ignored: the ontology is always the tenant's.

## Returns

`{exists, document_id?, status, confirmed, version, sections, partial?, classes,
relations, grammars, schemes, instances, issues?}`:

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
- `grammars` — each `{name, class?, patterns, resolve, issue?, used_as?}`.
  - A grammar with an `issue` is disabled and has no patterns.
  - `used_as` says how classes name it: `anchor`, `citation` or both.
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
