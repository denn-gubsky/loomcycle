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
