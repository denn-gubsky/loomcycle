---
name: content-signatures
description: SHA-256 content signatures on AgentDef + SkillDef rows for bundle-vs-deployed comparison.
---
Every persisted `agent_defs` and `skill_defs` row carries a
deterministic `content_sha256` hash of its content-bearing fields.
The hash answers one specific operator question: **"is the agent
(or skill) I have bundled in my Docker image identical to what's
deployed in this loomcycle, or do I need to push an update?"**

Without the hash, an operator running loomcycle in a container has
three options, all bad:
- Push agents unconditionally on every container start (wasteful;
  creates spurious new versions).
- Fetch the full Definition JSONB on every check and diff field-by-
  field (chatty; fragile to ordering changes; doesn't address skill
  body whitespace drift).
- Don't check; accept drift between bundle and deployment.

With the hash, the comparison is a single GET + a single string
equality check. The CI pipeline computes the hash once at image-
build time; the runtime container compares it on start-up; only on
mismatch does it push a new version.

## What the hash covers

For an **AgentDef**:

```
name, description, system_prompt, tools, skills, model,
provider, tier, effort, max_tokens, max_iterations, providers,
models, memory_scopes, memory_quota_bytes
```

For a **SkillDef**:

```
name, description, body, tools
```

Excluded from both (would defeat the "did the content change?" point):
`def_id`, `version`, `parent_def_id`, `created_at`, `created_by_*`,
`retired`, `bootstrapped_from_static`.

Also explicitly excluded from the AgentDef hash: `channels`,
`agent_def_scopes`, `skill_def_scopes`, `evaluation_scopes`. These
exist on the operator-yaml loader path but DO NOT round-trip through
`AgentDef set` / `fork` — they're operator-yaml-only ACL declarations
the substrate doesn't persist in the `agent_defs` row. If they
participated in the hash, a YAML-loaded agent and the same agent
pushed via the substrate would hash differently and the whole
comparison would falsely report drift.

## Hash format

`sha256:` + 64 lowercase hex chars (matches Docker image-digest
convention; leaves room for `sha512:` / `blake3:` later). Example:

```
sha256:9c6a6098efbad0bdde2cd0c777a70d97b204125c37004b3f80a7f5734cde2c03
```

The canonical encoding is deterministic across Go versions and
platforms: object keys are sorted, struct fields render in
declaration order, empty slices and zero ints normalise to "absent",
strings are UTF-8, system_prompt + body are trimmed of trailing
whitespace and have CRLF normalised to LF. Internal whitespace
(blank lines between paragraphs) is preserved.

A known-vector test pins the algorithm — any code change that moves
the hash on a fixed input fails the build, so silent drift is caught
at PR time.

## Operator workflow

The shape of the workflow is the same whether you use the CLI, the
TS adapter, the gRPC surface, or the MCP meta-tool. The CLI is the
simplest illustration:

```bash
# At image-build time (Dockerfile / CI):
loomcycle hash agent agents/researcher.md > /bundle/researcher.sha256
loomcycle hash skill skills/summariser     > /bundle/summariser.sha256

# At runtime container start-up:
LOCAL=$(cat /bundle/researcher.sha256)

# Ask the deployed loomcycle: do we match?
RESP=$(curl -sH "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  "$LOOMCYCLE_URL/v1/_agentdef" \
  -d "{\"op\":\"verify\",\"name\":\"researcher\",\"content_sha256\":\"$LOCAL\"}")

MATCHES=$(echo "$RESP" | jq -r .matches)
if [ "$MATCHES" = "true" ]; then
  echo "researcher in sync"
else
  echo "researcher needs push; pushing now"
  # Build the overlay from the bundled MD's frontmatter+body and
  # POST it as `{"op":"set", "name":"researcher", "overlay":{...}}`.
fi
```

The CLI accepts:
- `loomcycle hash agent <path-to-md>` — one MD file.
- `loomcycle hash skill <path-to-skill-dir>` — a directory containing
  SKILL.md.
- `loomcycle hash skill <path-to-skill-dir>/SKILL.md` — also accepted.

## The verify op

`AgentDef verify` / `SkillDef verify` take a name + a caller-supplied
hash; the tool reads the active row and answers:

```jsonc
// Input
{"op": "verify", "name": "researcher", "content_sha256": "sha256:..."}

// Output (in sync)
{
  "matches": true,
  "current_sha256": "sha256:...",
  "current_def_id": "def_abc",
  "version": 4,
  "name": "researcher",
  "deployed": true
}

// Output (drift)
{
  "matches": false,
  "current_sha256": "sha256:different...",
  "current_def_id": "def_def",
  "version": 4,
  "name": "researcher",
  "deployed": true
}

// Output (no active row)
{
  "matches": false,
  "current_sha256": "",
  "current_def_id": "",
  "version": 0,
  "name": "researcher",
  "deployed": false
}
```

Two invariants worth highlighting:

- **An empty `content_sha256` on the input NEVER matches.** If your
  CI step fails and you pass an empty string, the tool returns
  `matches: false` — never `matches: true` on an empty-string-equals-
  empty-string mistake.
- **`deployed: false` always implies `matches: false`.** Use the
  `deployed` field to discriminate "no active row" (deploy first
  time) from "drift" (push an update).

## Backfill for pre-v0.9.x rows

Rows that existed before this column was added — and rows restored
from a pre-v0.9.x snapshot — start with an empty `content_sha256`.
The boot-time backfill walks every NULL/empty row once at start-up,
computes the hash, and writes it back. The log line:

```
agent_defs: backfilled N rows with content_sha256
skill_defs: backfilled M rows with content_sha256
```

is emitted once per fresh upgrade. Subsequent boots find zero NULL
rows and the backfill is a no-op.

If the backfill ever fails on a row mid-way (corrupt Definition
JSONB, etc.), the partial progress is preserved and a warning is
logged. The remaining NULL rows are picked up on the next boot or
on a re-touch via `AgentDef set` / `fork`.

## Snapshot forward-compat

The v0.8.17 snapshot envelope's `AgentDefEntry` / `SkillDefEntry`
sections grow an optional `content_sha256` field. A v0.9.x snapshot
restored to a v0.9.x reader preserves the hash verbatim. A v0.8.x
snapshot restored to a v0.9.x reader leaves the column NULL on
restore; the boot-time backfill catches those rows on the next
start. Section version stays at "1.0" per the v0.8.17 additive-
fields rule.

A v0.9.x snapshot restored to a v0.8.x reader (downgrade) drops the
unknown field on decode; the older runtime never sees it. Safe.

## What this does NOT do

- **It is NOT an idempotent push helper.** `AgentDef set_if_changed`
  was considered and deferred — operators can compose `verify` + a
  conditional `set` themselves in two lines; baking the conditional
  into the substrate adds surface area without saving meaningful
  work.
- **It does NOT recompute the hash on every read.** Every row carries
  its persisted hash; the only places that re-sign are `set` / `fork`
  / `bootstrapStatic` / the boot-time backfill. If the JSONB
  definition is mutated outside those paths (manual SQL surgery),
  the hash falls out of sync until the row is naturally re-touched
  or you run a manual UPDATE to NULL the column + restart.
- **It does NOT include a fork-lineage Merkle tree.** Each row stands
  alone; lineage stays in `parent_def_id`.
- **It does NOT chain skill bodies through the agent hash.** An
  agent's `skills: [summariser]` field is part of the hash (the
  skill *name*), but the skill's body is NOT. If the operator
  changes a skill body, hash the skill separately + push that
  separately.

## References

- `help(topic="skills-evolution")` — the v0.8.22 SkillDef substrate
  that runs alongside this.
- `help(topic="loomcycle")` — the agent-definition + lineage model
  the hash sits on top of.
- `loomcycle hash agent <path>` / `loomcycle hash skill <path>` —
  the CLI helper (see `loomcycle help` for the full subcommand list).
