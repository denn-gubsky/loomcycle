---
name: Document/propose_entity
description: "Document op=propose_entity — suggest a new entity type, relation kind or concept for the tenant ontology; it stays inert until an operator accepts it."
---
`propose_entity` files a suggestion that the tenant ontology gain a new entity
type (or a subtype of an existing one) — or, with `section: relations`, a new
relation kind: a name for how two things are related — or, with
`section: schemes`, a new concept in a topic or classification scheme — or, with
`section: instances`, a real organisation, unit or post the tenant should know. The proposal is **inert**: no run sees
it and nothing changes until an operator accepts it in the console. It needs no
tenant grant and always goes to your own tenant's ontology, whatever `scope`
you pass — but **`scope` is still checked first**, so it must be a scope your
run can open (the default `user` needs a user id; otherwise pass `agent`).

## Arguments

- `name` (required) — the proposed type's name, e.g. `vehicle`.
- `title` — accepted in place of `name`.
- `parent` — an entity type that is already IN FORCE, by name, that this one is
  a kind of. Omit for a new top-level type. With `section: relations`, a
  relation kind the tenant declares, that this one is a narrower kind of.
- `section` — `classes` (the default) for an entity type, `relations` for a
  relation kind, `schemes` for a concept, `instances` for a real thing. A relation kind or concept can only be
  proposed where the tenant's ontology declares that section at all. A concept
  needs `parent`: the scheme, or the broader concept, it belongs under — by name
  or by its tag (`topics/risk-management`). An instance needs `parent`: the class
  it is an instance of (`organization`).
- `concepts` — with `section: schemes` and **no** `parent`: a whole NEW scheme
  named `name`, filed as ONE proposal. Each concept is `{name, meaning?,
  notation?, alt?, exact_match?, children?}`, `children` being narrower concepts
  of the same shape: up to 8 levels deep (deeper ones are lifted onto the
  eighth, with a warning) and at most 500 concepts in all (more is refused,
  naming the limit). Without a Schemes section in the ontology the proposal
  carries one, and accepting puts it in place.
- `header` — a new scheme's header: `{domain?, projects?, origin?, source?,
  version?}` — the knowledge domain it serves, the classes whose things it
  arranges, `adopted`, `declared` or `generated`, and an adopted scheme's
  source and edition. Carried as data, never in the text, and written as
  directives only when an operator accepts — so is each concept's notation,
  alternative labels and exact match. Nothing else becomes a directive: any
  `@` line in `body` or a `meaning` stays quoted text.
- `body` — your evidence: counts and a few example facts, at most 4000 bytes.
  For a new scheme, its meaning.
- `scope` — see above.

## A new class or relation kind with its data

A class or relation add (no `change`) may carry structured data beside its
evidence. Any of these makes it one:

- `meaning` — the new entry's meaning; any `@` line stays quoted text.
- `directives` — the new entry's directives, an object as for an edit. Only a
  relation kind's `domain` and `range` may be proposed; a class takes none.
  Every other directive (`locked`, `acl`, `retention`, `memory_scope`,
  `auto_assert`, `identity` …) is refused by name: an operator adds it after
  accepting.
- `evidence` — up to 5 quotes, each `{chunk_id, quote, part?}` naming a chunk of
  the tenant scope; a quote that does not occur in its chunk is refused.
- `signal` — the building signal the proposal answers: `undeclared_type`,
  `undeclared_kind` or `invalid_ends`.

They travel in the proposal's fields (`entry_proposal`), never in its text,
with a plan computed when it is filed (the ontology's fingerprint and the
tenant-scope rows already using the name) and the run that filed it. The answer
adds `risk` (always `none` for an addition). Accepting writes the entry's text
from the directives and the meaning, checked again — not from `body`, which
stays the evidence the reviewer read. A run may hold at most 10 such open
proposals; the eleventh is refused with `proposal_cap`.

## Changing or retiring an entry

With `change`, the proposal is about an entry ALREADY in force — a class,
relation kind, scheme, concept or instance — instead of a new one. It is
just as inert: the entry, every read and every run are unchanged
until an operator accepts it.

- `change` — `edit` or `retire`. A retired entry is not deleted: nothing new
  may use it, and what already uses it stays readable.
- `entry` (required with `change`) — the entry's chunk id from
  `describe_ontology`, or its name (a concept's tag) when that names one entry;
  add `section` to narrow a name. A section header, the root and a proposal are
  not entries.
- With `edit`, at least one of:
  - `name` — its new name;
  - `parent` — its new parent in its own section, by chunk id or name;
  - `meaning` — its new meaning; any `@` line stays quoted text;
  - `directives` — its COMPLETE new directive set: an object of directive name
    to value — a string, `true` for a flag such as `symmetric`, or a list for a
    directive that repeats (`alt`); `{}` clears them. Only the directives the
    entry's section takes, checked as the section reads them; never `locked`.
- `body` — your reason and evidence, as for any proposal.

A retire names only `entry`. The proposal is filed under the entry's section,
titled `edit: <name>` or `retire: <name>`, and the change travels as data in
its fields (`change_proposal`), never in its text. An operator accepting one
applies it to the entry in place, keeping its id: a retire retires the entry and
everything under it; an edit renames it, moves it, and writes its body as the
directives above the meaning (a class's field bullets stay as they are — a
proposal never adds or removes a field). The change is checked again against
the ontology as it then stands, and refused if the proposal no longer passes
or was edited into something it would not pass. A class that something rests
on is not renamed until the runtime can carry the new name through stored data;
a relation or concept is. The accepted proposal is then deleted, and other open
proposals of the same kind for the same entry are rejected. Rejecting works as
for any proposal.

Refused: an entry that is not in force; a retired entry (it is brought back by
an operator clearing its status, not changed by a proposal); retiring one
already retired; a new name or parent that is a retired entry, or a name
another entry of the kind already has; an entry carrying `@locked`, or for a
retire, rename or move one with a locked entry under it; a second open
proposal of the same kind for the same entry.

To file a published classification, use `import_scheme`, which reads a Markdown
outline or SKOS and files the same kind of proposal.

## Returns

`{proposed, chunk_id, parent_id, note}` — `note` says it is filed and not in
force. A change or retire proposal adds `change`, the data it carries. A new scheme adds `concepts` (how many), `header`, `warnings` when
something was changed to fit, and `carries_section: true` when the ontology
has no Schemes section yet.

## Errors

- `propose_entity: name is required ...`.
- `propose_entity: body is N bytes, over the 4000-byte limit ...` — shorten
  the evidence.
- `propose_entity: this tenant has no ontology document yet ...` — an operator
  must open the ontology once. Retrying is pointless.
- `propose_entity: <name> is already in force ...` — propose a subtype of it
  instead, or use it as it is.
- `propose_entity: <name> is already proposed` / `... rejected` — an operator
  has seen it. Do not file it again.
- `propose_entity: no entity named <parent> is in force ... (have: ...)` — pick
  a parent from the list it gives.
- `propose_entity: this tenant's ontology declares no Relations section ...` —
  relation kinds are not used here; describe the relation in prose.
- `propose_entity: <name> is a standard relation kind and already valid` — use
  it as it is.
- `propose_entity: ... is not an entry in force ...` — pass an entry's chunk id
  from `describe_ontology`.
- `propose_entity: already_retired: ...` / `... exists and is retired ...` —
  ask an operator to bring it back; do not propose a change to it.
- `propose_entity: entry_locked: ...` — the entry is locked; only an operator
  changes it.
- `propose_entity: already_proposed: ...` — an open proposal of the same kind
  exists for the entry; an operator decides.
- `propose_entity: directives: ...` — a directive the section does not take, a
  value its parser cannot use, or `locked`; on an add, any directive but a
  relation's `domain` and `range`.
- `propose_entity: proposal_cap: ...` — this run already has 10 open proposals
  carrying data; stop and let an operator review them.

## Examples

Propose a subtype of an existing type, with evidence:

```json
{"op": "propose_entity", "name": "vehicle", "parent": "object", "body": "14 facts mention cars, vans or bikes the user owns, e.g. 'Ada drives a 2019 Škoda Octavia'."}
```

```json result
{"proposed": "vehicle", "chunk_id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "parent_id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "note": "Filed as a suggestion, not in force. ..."}
```

A relation kind, narrower than one the tenant already declares:

```json
{"op": "propose_entity", "section": "relations", "name": "supervises", "parent": "manages", "body": "6 facts say one person supervises another's thesis."}
```

A relation kind with its meaning, domain and range, and a quote it rests on:

```json
{"op": "propose_entity", "section": "relations", "name": "approves", "signal": "undeclared_kind", "meaning": "One party approves another's document.", "directives": {"domain": "person", "range": "Policy"}, "evidence": [{"chunk_id": "9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e", "quote": "the board approves the policy"}], "body": "14 proposed statements use the kind."}
```

```json result
{"proposed": "approves", "chunk_id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "parent_id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "risk": "none", "note": "Filed as a suggestion, not in force. ..."}
```

A concept, narrower than one a topic scheme already has:

```json
{"op": "propose_entity", "section": "schemes", "name": "cloud outsourcing", "parent": "topics/risk-management/outsourcing", "body": "11 documents discuss cloud providers under outsourcing."}
```

An instance — a real organisation the tenant keeps meeting:

```json
{"op": "propose_entity", "section": "instances", "name": "National Bank of Ukraine", "parent": "organization", "body": "Cited as the issuer in 23 documents."}
```

A whole new scheme with its header and concepts, as one proposal:

```json
{"op": "propose_entity", "section": "schemes", "name": "Aircraft systems", "body": "Chapters of the maintenance manuals.", "header": {"domain": "aircraft maintenance documentation", "projects": ["manual"], "origin": "generated"}, "concepts": [{"name": "Hydraulics", "notation": "29", "children": [{"name": "Pumps"}]}, {"name": "Avionics"}]}
```

A retirement, and a rename with a new directive set:

```json
{"op": "propose_entity", "change": "retire", "entry": "depends_on", "section": "relations", "body": "No statement of this kind in a year; gated_by covers it."}
```

```json
{"op": "propose_entity", "change": "edit", "entry": "3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c", "name": "oversees", "directives": {"functional": true, "domain": "person"}, "body": "The 2026 charter renames the relation."}
```

A top-level type from an agent with no user on its run:

```json
{"op": "propose_entity", "name": "medication", "scope": "agent", "body": "9 facts name prescriptions and dosages."}
```
