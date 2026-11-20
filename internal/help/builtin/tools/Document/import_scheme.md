---
name: Document/import_scheme
description: "Document op=import_scheme — file a published classification (a Markdown outline or SKOS in RDF/XML) as one proposed concept scheme with @origin adopted; nothing is in force until an operator accepts it."
---
`import_scheme` turns a classification that already exists outside the tenant —
a library's subject headings, an industry's chapter scheme, a file plan — into
**one proposal** in the tenant ontology: the scheme, its header and every
concept beneath it. It writes nothing in force. An operator reviews it on the
Ontology page and accepts or rejects the whole scheme.

It asks what `propose_entity` asks: no tenant grant, and it always goes to your
own tenant's ontology. **`scope` is still checked first**, so it must be a
scope your run can open (the default `user` needs a user id; otherwise pass
`agent`).

The scheme's header says it is adopted: `@origin adopted`, with `@source` and
`@version` from `header`. Every concept of an adopted scheme reports
`origin: "adopted"` in `describe_ontology`, so a later pass knows not to rename
or move it. Accepting also writes `@locked` on every concept (not on the
scheme entry), so no one — the operator included — renames, moves, retires or
deletes one until an operator removes the line in the editor. A lock holds
every entry above it, so the scheme itself cannot be renamed or retired while a
concept of it is locked. Its text stays editable, and a local concept can still
be added under an adopted one.

## Two inputs

Pass exactly one.

**`markdown` — an outline.**

```text
# Subject headings
Headings for the library catalogue.

- `QA` Mathematics
  alt: Maths
  note: The science of number and space.
  - `QA76` Computer science
    alt: Computing
    exact: http://id.loc.gov/authorities/classification/QA76
## Physics
notation: QC
- `QC1` Mechanics
```

- `# name` — the scheme's name: at most one, before any concept. Optional when
  you pass `name`.
- Text before the first concept — the scheme's meaning.
- A concept is a list item (`- `, `* ` or `+ `) or a heading. A list item
  indented deeper than the one above it is narrower than it. `##` is a top
  concept, `###` narrower than the `##` above it, and so on; list items after a
  heading are narrower than that heading.
- An optional notation comes first, in a code span: `` `QA76` Computer science``.
- Lines under a concept: `alt:` an alternative label (repeat it for more),
  `note:` its meaning, `exact:` an exact match (a URI), `notation:` its
  notation. Any other text under a concept is added to its meaning.
- A ```` ``` ```` fence is skipped.

**`skos_xml` — SKOS in RDF/XML.** Read with the standard library: concepts and
the scheme as typed elements (`skos:Concept`, `skos:ConceptScheme`) or as
`rdf:Description` with `rdf:type`, nested or not. Taken: `prefLabel`,
`altLabel`, `notation`, `definition` (else `scopeNote`) as the meaning,
`broader` / `narrower` / `topConceptOf` / `hasTopConcept` for the tree,
`inScheme`, and `exactMatch`. Everything else is skipped. Turtle is not read.

- `language` picks the labels and notes: a value tagged with it (`en` also takes
  `en-GB`), else an untagged one. A concept with neither keeps a label in
  another language, and the answer warns. Default: the tenant's annotation
  language, else `en`.
- A concept with two broader concepts is put under the first; the answer warns.
- A file with several concept schemes needs `scheme`: the one to import, by IRI
  or label.
- A DOCTYPE is refused, and no entity is expanded.

## Limits

- At most **500 concepts** per call. A larger scheme is refused with the limit
  named: it needs a document of its own, which is not supported yet. Import its
  upper levels instead.
- Concepts may nest **8 levels** below the scheme. Deeper ones are lifted onto
  the eighth level, beside their parent, and the answer warns — as the reader
  does with a document nested too deep.
- The input is at most 2 MB. A name is at most 200 characters, a meaning 1000.
- A notation or exact match must be one word; one with a space is dropped with
  a warning.
- Two concepts whose tags (`<scheme>/<path>`) would be the same are refused.
- A scheme whose name is already declared, proposed or rejected is refused.

## Arguments

- `markdown` or `skos_xml` — the scheme (above).
- `name` — the scheme's name; overrides the outline's `#` title or the SKOS
  scheme's label.
- `body` — the scheme's meaning; overrides the outline's or the SKOS scheme's.
- `header` — `{domain?, projects?, source?, version?}`. `origin` may be left out;
  anything but `adopted` is refused.
- `language`, `scheme` — SKOS only (above).
- `scope` — see above.

## Returns

`{proposed, chunk_id, parent_id, concepts, header, carries_section?, warnings?,
note}` — `chunk_id` is the proposed scheme. `carries_section: true` says the
ontology has no Schemes section yet; accepting puts one in place.

## After it is filed

The proposal is listed with the ontology's proposals as one entry with a
`scheme` block: its header, its concept count and its outline. An operator
accepts it with `POST /v1/_ontology/proposals` (`{"chunk_id": …, "action":
"accept"}`) or on the Ontology page. Accepting checks the stored data again,
writes the header and each concept's notation, alternative labels and exact
match as directives, adds `@locked` to each concept, and puts the scheme and every concept in force, each
keeping its chunk id. Rejecting keeps the scheme as a tombstone and changes
nothing in force.

## Examples

Import an outline:

```json
{"op": "import_scheme", "scope": "agent", "markdown": "# Aircraft systems\n- `29` Hydraulic power\n  - `29-10` Main\n- `34` Navigation\n", "header": {"domain": "aircraft maintenance documentation", "projects": ["manual"], "source": "the publisher's chapter list", "version": "2024.1"}}
```

```json result
{"proposed": "Aircraft systems", "chunk_id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "parent_id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "concepts": 3, "header": {"domain": "aircraft maintenance documentation", "projects": ["manual"], "origin": "adopted", "source": "the publisher's chapter list", "version": "2024.1"}, "note": "Filed as one suggestion, not in force. ..."}
```

Import a SKOS file in Ukrainian:

```json
{"op": "import_scheme", "scope": "agent", "skos_xml": "<rdf:RDF …>…</rdf:RDF>", "language": "uk", "header": {"source": "https://example.org/fileplan.rdf"}}
```
