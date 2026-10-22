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
  guarantee is real. Those writes are held in memory: a call may write at
  most 64 MiB in total to in-memory files (unless the operator changed it),
  and deleting a file does not give it back. Every file or directory created
  counts too. Past the limit a write fails with `no space left on device`, so
  keep scratch files on a read-only volume small. Changing a file of the
  volume (chmod, touch, mv, ln, appending) copies it into that memory, at its
  full size; reading it unchanged does not.

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
2. **Per agent:** add `Bashbox` to the agent's `tools`.

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

## Host-command fallback (operator opt-in)

Commands gbash doesn't implement — `git`, `gh`, `npm`, … — normally fail with
`command not found`. An operator can allowlist specific ones to run on the
**real host shell** instead:

- `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS=git,gh` — only these names escape the
  sandbox; **every other command still runs sandboxed** (so `git status; curl …`
  runs git on the host but `curl` stays in the sandbox).
- `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV=GH_TOKEN,HOME,SSH_AUTH_SOCK` — host
  env vars passed into those host commands.
- `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS=git:GITHUB_TOKEN,gh:GH_TOKEN` —
  stored credentials resolved for your tenant/user and passed into those host
  commands. `cmd:NAME` hands the credential to that one command only; a bare
  `NAME` hands it to every allowlisted command. Your agent's
  `shell_credentials` may keep only some of these, or none; `Context op=self`
  lists the names your host commands actually receive. A name your agent gave
  up is withheld even when the operator also admits a host variable of that
  name; `Context op=self` lists those under `bashbox_fallback_withheld_env`.

These variables are injected only into the host command, so the sandbox's own
`env` does not list them. They are **not** hidden from you: an allowlisted host
command can print its environment (`git -c alias.x='!env' x`), and anything it
starts (a git alias, hook or credential helper) inherits them. Treat any
credential a host command is given as one you can read, and never print or
store it — the saved transcript may mask it, but the tool result you see, and
everything you send on from it, carries it.

Fallback commands **require a read-write volume** (a real host process can't
honor the read-only overlay), run with their working directory set to the host
path for your current dir, and have host filesystem + network access. They are
off by default; this tool's description lists which (if any) the operator
enabled.

## Caveat

gbash is **alpha** and pinned to an exact version in loomcycle's `go.mod`. The
opt-in posture is the escape hatch: if a gbash bug surfaces, drop `Bashbox`
from the agent's `tools` (or unset the env flag) and fall back to the
file tools or `Bash`.

Bashbox is a strong layer against an untrusted prompt, but it runs inside the
loomcycle process and shares its memory with every other run. Operators
serving mutually hostile tenants from one process should also run loomcycle in
a container until gbash reaches 1.0. A write to a read-only volume is held in
memory with no size cap until the command ends, so do not write large or
unbounded output to a read-only volume.
