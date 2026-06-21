---
name: bashbox
description: Bashbox — a TRUE in-process shell sandbox (gbash) that spawns no OS process, roots paths at your volume, has no network, and HONORS read-only volumes. The sandboxed alternative to Bash.
---
Bashbox runs a shell command in a **true sandbox**. It is the isolated
counterpart to `Bash`: where `Bash` shells out to a real `/bin/sh` (and so is
"restricted, not isolated" — see the `Bash` warning), Bashbox executes the
command **in-process** via gbash, a pure-Go shell that reimplements the common
coreutils against a **virtual filesystem** and spawns **no operating-system
process**.

That changes what the runtime can honestly promise:

- **No OS process.** There is no `/bin/sh` fork, no `PATH` lookup, no way to
  reach a host binary or a setuid escalation. Unknown commands (e.g. `git`) are
  refused, not shelled out.
- **No host filesystem escape.** Every path is rooted at the volume you run in;
  there is no absolute-path back door to the host tree.
- **No network.** Egress is off — `curl` and friends are refused. (Opt-in,
  operator-allowlisted egress is a planned follow-up; v1 has none.)
- **Read-only volumes are HONORED.** This is the key difference from `Bash`.
  `Bash` *refuses* a `ro` volume because a real shell defeats path-confinement.
  Bashbox mounts a `ro` volume under an **in-RAM write overlay**: writes
  succeed *inside the run* (so a script can use scratch files) but **never
  touch the host tree** — they are discarded when the call returns. The ro
  guarantee is real.

## When to use it

Reach for Bashbox when you want shell ergonomics (pipes, `grep`/`sed`/`awk`/
`find`/`jq`, loops, redirection) **without** handing the model a real host
shell — especially against a **read-only** volume, or any deployment that is
not already wrapped in a container/VM. Use `Bash` only when you genuinely need
a host binary or host process semantics that gbash doesn't provide (and accept
that `Bash` is not a sandbox).

## Enabling it

Opt-in, exactly like `Bash`:

1. **Per deployment:** `LOOMCYCLE_BASHBOX_ENABLED=1`.
2. **Per agent:** add `Bashbox` to the agent's `allowed_tools`.

An agent still needs a bound `volumes:` volume (sandbox-by-default applies —
no volume, no filesystem). Unlike `Bash`, the bound volume may be `ro`.

## Input

- `command` (required) — the shell command. Use paths **relative** to your
  volume root (`ls .`, `grep -rn foo src`).
- `volume` (optional) — which bound volume to run in; omit for your default.
  **`ro` volumes are allowed.**
- `timeout_seconds` (optional) — per-call cap, ceiling 300s.

Returns combined stdout+stderr; a non-zero exit is surfaced as an error (with
the output preserved) so you can self-correct.

## Command coverage

gbash ships the common coreutils as built-ins (`echo`/`cat`/`ls`/`head`/`tail`/
`wc`/`grep`/`sed`/`find`/`sort`/`uniq`/`cut`/`tr`/`test`/`mkdir`/`cp`/`mv`/`rm`/…),
plus shell control flow (pipes, `&&`/`||`, `for`/`while`, command substitution,
parameter expansion). Bashbox additionally bundles the pure-Go **`awk`** and
**`jq`** contrib commands. A command-coverage spike measured **97.4%** parity
with `/bin/sh` on a representative agent corpus; the lone gap is `git` (a
sandbox should refuse it anyway — it needs host credentials and mutates a real
repo).

Some cosmetic differences are expected: `wc`/`uniq` column padding differs, and
`pwd` reports the sandbox mount root rather than the host path.

## Caveat

gbash is **alpha** and pinned to an exact version in loomcycle's `go.mod`. The
opt-in posture is the escape hatch: if a gbash bug surfaces, drop `Bashbox`
from the agent's `allowed_tools` (or unset the env flag) and fall back to the
file tools or `Bash`.
