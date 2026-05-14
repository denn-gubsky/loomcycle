# Contributing

Loomcycle is **Apache-2.0 open source from day one**, and external code
contributions will open with the **stable v1.x release**. This document
covers how you can contribute today and what's coming at v1.x.

## Today (v0.8 → v0.9 → v1.0)

The core primitives — Memory, Channel, AgentDef/Evaluation, Context,
LoomCycle MCP — are stabilizing, and the high-load capacity work
(per-tenant fairness, OTEL, multi-replica HA) is in active design.
While that work is in flight, we're keeping the contributor surface
narrow so the API and behaviour can settle without churning third-party
patches between review rounds.

We welcome these contributions right now:

- **Bug reports** for clear-cut runtime defects — panics, data
  corruption, wire-shape regressions against the documented contract,
  security issues. File an issue with a minimal reproduction; we triage
  these actively.
- **Security disclosures** — please send privately via the email in the
  project's repository profile rather than filing a public issue.
- **Downstream consumers** — building against the wire surface
  (`/v1/*` HTTP, gRPC, the embedded Web UI) is exactly what we want.
  Production integrations inform v1.0 design. Open an issue describing
  your use case if it's not covered well, or just tell us you're
  building so we can factor in your shape.
- **Forks** — Apache-2.0 means fork freely. If your fork diverges,
  that's fine — we won't chase compatibility with downstream forks
  during v0.8/v0.9/v1.0 development, but you have all the freedom you
  need under the license.

## Code PRs during the stabilization window

If you've written a code change before v1.x:

1. It will be acknowledged with a link to this document.
2. It will be closed rather than merged — **not as a rejection of the
   idea**, but because the surrounding code is still moving and merging
   now would put a maintenance burden on both of us.
3. You're warmly invited to re-open the same change once v1.0 ships
   and this document is replaced with the full contributor guide.

If your PR is a **security fix**, mark it as such — those are reviewed
and merged during the stabilization window.

## What's coming at v1.0

When v1.0 ships — triggered by shipped LoomCycle MCP capstone + v0.9.x
capacity work + v1.0 distribution channels (Homebrew, Docker, Helm) +
integration recipes, not a calendar date — this document is replaced
with a full contributor guide covering:

- The architect → plan → branch → code → tests → review → merge chain
  (already documented for maintainers in `CLAUDE.md`).
- The RFC process for non-trivial features (RFC under
  `doc-internal/rfcs/<feature>.md` before code).
- Code style, commit message conventions, test discipline, security
  rules.

Until then: thanks for your interest, and please bear with us while we
stabilize the foundation.
