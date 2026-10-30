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

**Sections, typed fields and document classes.**

Everything above still works as written. When you want more than a flat list of
entity types, give a top-level heading a `@section` line as its first directive.
Nesting under it then means what that section says:

- **classes** — the types you have been writing, with nesting as subclassing.
- **relations**, **schemes** and **instances** — relation kinds, topic and file-plan
  schemes, and your organisation's real units. Reserved for upcoming versions;
  declaring them now is harmless.

Inside a classes section, a field can say what it holds, and a class under
`Document` describes a kind of *document* rather than a kind of thing memory records:

```markdown
## Classes
@section classes

### Document
- `title` text @required

#### Policy
@identity
- `owner` text @required — who maintains it
- `issued` date
- `state` enum(draft, in_force, retired)

### Provision
- `anchor` anchor @identity — its article or clause number
```

Field types are `text`, `number`, `date`, `enum(…)`, `concept(scheme)`,
`ref(Class)` and `anchor`; a field with no type is text. `@required` and a value of
the wrong type produce warnings on the write, never a refusal. `@identity` makes the
field part of the thing's identity, and a bare `@identity` line means "its title
is its identity" — two chunks with the same identity are refused. Document classes
describe documents; they are never listed to the agents as entity types.

A document class can say where its documents define their terms:
`@definitions (?i)^definitions\b` marks every part whose title matches the pattern,
and everything under it, as a definitions passage, so a link from there reads as
"defines" rather than "uses". A subclass inherits it.

**Relation kinds.** A `@section relations` heading names the kinds of link agents
may record between two things. Declaring one — even an empty one — switches on the
standard kinds (`cites`, `based_on`, `amends`, `implements`, `related`, …) for every
link, and makes a link of an undeclared kind an error when written by hand. Nesting
makes a narrower kind, so a search for `changes` also finds `amends`:

```markdown
## Relations
@section relations

### references
#### cross_refers
A reference from one rule to another in the same rulebook.

### stricter_than
Sets a stricter version of the same requirement.
@domain Provision
@range Provision
@inverse looser_than
@asymmetric
@qualifiers aspect(security|privacy|operational)
@discriminator aspect
```

`@domain` and `@range` name the classes at either end; `@inverse` is how the
link reads from the other end; `@qualifiers` lists what a link may record about
itself, and `@discriminator` the one that makes two links between the same pair
distinct. Only the kinds you declare (or adopt from the standard set on the
Ontology page) are listed to the agents.

**Concept schemes.** A `@section schemes` heading holds the vocabularies documents
are classified by — topics, a ranking, a file plan. Each heading under it is a
scheme, and each heading under a scheme is a concept; a nested concept is narrower
than the one above it, and inherits nothing:

```markdown
## Schemes
@section schemes

### topics
@render
#### risk-management
@notation RM
##### outsourcing
@alt vendor management

### file-plan
#### compliance
@retention 10y
@acl compliance
```

Confirming gives every concept a tag such as `topics/risk-management/outsourcing`;
a document anywhere is classified under a concept by carrying its tag, and a
search for `topics/risk-management` also finds everything narrower. `@render`
lists a scheme to the agents. A file plan's `@retention` and `@acl` are recorded
and shown on the Ontology page but not enforced yet.

**Instances.** A `@section instances` heading lists the real organisations, units
and posts the agents should know by name. Each heading under it is one thing; `@a`
names its class, and `@nesting` says how the headings under it relate to it:

```markdown
## Instances
@section instances

### Acme Bank
@a organization
@nesting sub_organization_of
#### Compliance Department
@a OrganizationalUnit

### Head of Compliance
@a Post

### Compliance Officer
@a Post
@reports_to Head of Compliance
```

Confirming adds each one to memory as the subject facts are recorded about — the
same subject memory would have created on its own, so nothing is duplicated — and
links them as declared. Renaming one here keeps its identity; removing one never
deletes it from memory.

**Where a type's facts are stored.**

A type may declare which memory scope facts about that kind of thing belong in:

    ## service
    - `@memory_scope` tenant
    - `name` — what people call it

`tenant` means facts about a service are written to the plane **every user in this
tenant reads**, instead of into the scope of whoever happened to mention it. `user`
means they stay with the person. Declaring nothing — the default, and what every type
below does — leaves facts exactly where they are written today.

A subtype inherits the declaration, so `organization → tenant` is written once.

Three things it will NOT do, so you can declare one safely:

- It never places a fact about **you** that was learned from *your own* conversations.
  Add your names to the Identity section of your own profile document and those facts
  stay yours whatever their type says. ⚠️ It cannot stop someone ELSE from recording the
  same thing about you: to them you are a third party, and their copy is placed. If that
  matters, declare the impersonal types (`organization`, `project`, `service`) and leave
  `person` alone.
- It never places a fact whose subject is typed **inconsistently** — one thing recorded
  as two different types is a thing the store cannot identify, and it is left alone
  with a note rather than filed under a guess.
- It does nothing at all unless the agent that writes memory is granted the scope, on
  **both** `memory_scopes` and `sql_scopes` — a fact is stored twice, and both halves
  have to be able to land in the same place. The bundled memory agents hold those
  grants already, so on a stock deployment this declaration is the only switch; a
  consolidator you configured yourself needs them added.

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
