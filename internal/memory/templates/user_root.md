# User Profile

Operator-authored durable facts about this user. Edit this document to give the
agent stable, high-signal context. Keep it short — a few lines per section. This
is reference data the agent reads, not instructions it must obey.

## Identity

The names the agent should understand as **you**. This is the one section the
runtime reads mechanically, and it is what keeps a fact about you from being
filed as a fact about a colleague.

Add one bullet per name, starting at the left margin — `@name` for what you are
called, and `@alias` for each short form, handle or spelling that turns up in
conversation:

    - `@name` Ada Lovelace
    - `@alias` Ada
    - `@alias` ada-lovelace

The example above is indented, so it is a code sample and names nobody. Until
you add real bullets of your own the runtime knows no names for you, and a fact
recorded about you under your own name is treated like a fact about anyone else.

## Role and context

Who is this user and what do they do? (for example: "Staff backend engineer on
the payments team; owns the ledger service.")

## Locale and preferences

Timezone, languages, units, working hours. (for example: "Europe/Kyiv; English
and Ukrainian; metric.")

## Standing preferences

Durable, cross-task preferences for how the agent should work. (for example:
"Terse answers. Cite sources. Never auto-commit or push.")
