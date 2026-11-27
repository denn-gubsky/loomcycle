# dev-workspace bundle

`dev/exec`, a deterministic code-js agent (no model, zero tokens), runs a
caller's JSON command envelope in a workspace on a workspace worker (RFC GW):
it opens a workspace, writes the envelope's files, runs its commands in order
and reads back the files it names. The `dev/exec-usage` skill tells a
delegating agent how to call it.

It replaces the `sandbox` + `dev-exec` pair, removed in v1.172.0 with the
builder sidecar (RFC GW P15). Same agent name, same result shape: select
`dev-workspace` where `sandbox,dev-exec` stood (`LOOMCYCLE_PRESETS` naming
either removed bundle now fails the start and names `dev-workspace`).

- **Needs workspaces on:** `workspace.mode` `local`, `remote` or
  `local+remote`, with a worker for the run's tenant (`docs/WORKSPACES.md`).
- **Network:** only the read-only `pypi`, `pypi-files` and `npm` routes of the
  `workspace` preset, which this bundle includes, so `pip install` and
  `npm install` work and nothing else is reachable.
- **No session across calls:** a workspace is released when dev/exec's run
  ends. To keep files between calls, hand dev/exec a fork of your own
  workspace (`Agent op=spawn ... workspaces=[{fork: "<name>"}]`) and name it in
  the envelope's `workspace`.
- Old envelope fields with no workspace counterpart (`session_id`,
  `keep_open`, `close`, `network`, `expose`, `cpu`, `mem_mb`, `pids`,
  `tmpfs_mb`, `git_setup`, `max_output_bytes`) are reported as ignored in
  `final_text`.

`LOOMCYCLE_PRESETS=base,dev-workspace`
