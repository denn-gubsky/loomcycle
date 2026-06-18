---
name: volumes
description: filesystem volumes — the named ro/rw roots your file/exec tools resolve paths against, and the optional `volume` tool argument.
---
A **volume** is a named filesystem root your file/exec tools
(Read / Write / Edit / Glob / Grep / Bash / NotebookEdit) resolve paths
against. The operator binds you to a set of volumes; each is either
read-write (`rw`) or read-only (`ro`). Volumes are the *only* way you get
filesystem access — they let one runtime confine different agents to
different working trees. **If you're bound to no volume, every file/exec
tool refuses: you have no disk access.**

## Seeing your volumes

Call `Context op=self`. When you're bound to volumes it reports a
`volumes.bindings` list — each entry's `name`, `path`, `mode` (`ro`/`rw`),
and whether it's the `default`. If you're bound to no volume it reports
`filesystem: "none — no volume bound"` — ask the operator to bind one
(file/exec tools will refuse until then).

## The `volume` tool argument

Read / Write / Edit / Glob / Grep / Bash / NotebookEdit accept an optional
`"volume"` string:

- **Omit it** → the call uses your *default* volume (the one marked
  `default`, or your sole binding when you have exactly one). If you have
  several bindings and none is the default, the call errors and lists the
  names — pick one explicitly.
- **Name one** → that volume is used. Naming a volume you aren't bound to
  errors and lists the volumes you *are* bound to.

Paths are RELATIVE to the chosen volume's root (e.g. `src/main.go`); `~`
is not expanded, and an absolute path is accepted only if it still
resolves inside that root. A `..` that climbs out of a volume is rejected
— you cannot reach one volume from another.

## Read-only vs read-write

- **Read / Glob / Grep** work on any volume you're bound to.
- **Write / Edit / NotebookEdit** require a `rw` volume — a `ro` target is
  refused.
- **Bash** also requires a `rw` volume. It cannot enforce read-only (a
  shell can write via absolute paths and redirection), so it refuses a
  `ro` volume rather than pretend otherwise. Bash's working directory is
  set to the chosen volume's root.

## Sub-agents

When you spawn a sub-agent (the `Agent` tool), the child's volumes are the
intersection of what the child declares and what *you* currently hold —
narrow-only. A child can never reach a volume you lack, and where both of
you hold a volume, the more restrictive mode (`ro`) wins. This is the same
narrowing the host allowlist uses, applied to the filesystem.
