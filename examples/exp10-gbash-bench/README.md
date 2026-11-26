# exp10 — Bash vs Bashbox benchmark

Two code-js agents run the same file-discovery operations on a clone of the
loomcycle repository: one through the `Bash` tool (a host subprocess), one
through `Bashbox` (the in-process gbash sandbox, RFC AJ). Each operation's
wall-clock time comes back as JSON, and `work/exp10_run.py` renders the
comparison.

**It measures what a future release removes.** `Bash` and Bashbox are
deprecated (RFC GW §9), and RFC GW P15 removes both, with this example.
Commands now run in a workspace on a workspace worker; see
`docs/MIGRATING-FROM-BASH.md` and `docs/WORKSPACES.md`. Kept for comparison
only: boot warns that `LOOMCYCLE_BASH_ENABLED` and
`LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS` are deprecated.

Start: `LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh`. Bench, in a second
terminal: `python3 work/exp10_run.py`.
