# Tenant Ontology

The entity types this deployment records memory against. Everything here is
yours to edit.

**This document does nothing until you confirm it.** While its status is
`draft`, agents use the standard types alone and ignore what is written below.
Set the status to `confirmed` when you are happy with it, and these definitions
apply from the next run onward.

The standard types are always available and do not need repeating here:
`person`, `object`, `location`, `event`, `organization`, `preference`, `fact`.
Add a type below only when your work needs something they do not cover — or
reuse a standard name to override its fields.

**Nesting makes a subclass.** A `### internal-project` under `## project` is a
kind of project: it inherits every field declared above it and adds its own, so
you never restate them. Nest as deep as four levels. To subclass a *standard*
type, first give it a `##` heading of its own here: that overrides the standard
one, and you can then nest beneath your copy.

An inherited field cannot be removed — a subclass that lacks its parent's fields
is not a subclass. If you want a type without them, make it a sibling instead.

`preference` and `fact` are the memory tier's own types and always stay
top-level. You may nest your types *under* them, but nesting them under one of
yours is ignored. Names work best lowercase, as `a-z0-9-_` — a name with spaces
still works, it just reads as prose inside a prompt.

## project

A body of work with a lifecycle of its own. Worth its own type when memory
needs to attach decisions and constraints to something narrower than the
organization.

- `name` — what people call it in conversation, not its formal title
- `status` — active, paused, shipped, abandoned
- `repository` — where the code lives, if it has any
- `owner` — the person accountable for it

## incident

Something that went wrong and was worth remembering. A useful type because the
lesson usually outlives the event, and a bare `event` loses the cause.

- `summary` — one sentence on what happened
- `occurred_at` — when it started
- `cause` — what turned out to be responsible
- `resolution` — what actually fixed it

## constraint

A rule that limits how work may be done. Distinct from a `preference`: a
preference can be overridden with a reason, a constraint cannot.

- `statement` — the rule, in the imperative
- `scope` — what it applies to
- `rationale` — why it exists, so a later reader can tell whether it still does

---

Delete the examples above once you have your own. An ontology of three types you
actually use is worth more than a dozen you copied.
