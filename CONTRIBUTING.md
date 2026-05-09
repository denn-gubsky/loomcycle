# Contributing

> **Loomcycle is closed to external contributions until the stable v1.x release ships.** Pull requests, issues that propose new features, and unsolicited patches will be acknowledged but not reviewed or merged. This policy is in effect for the duration of active v0.8 / v0.9 / v1.0 development.

## Why

Active development on the framework primitives (Memory, Channel, LoomHelp, LoomCycle MCP) and the high-load capacity sweep (per-tenant fairness, OTEL, multi-replica HA) is moving faster than third-party PR review can responsibly keep up with. Reviewing PRs against a moving target produces churn for both sides — the contributor's work bit-rots between review rounds, and the maintainer's roadmap fragments around drive-by additions.

Reopening contributions is a deliberate phase change tied to the v1.0 stable release, not a calendar date. The trigger is: shipped LoomCycle MCP capstone (v0.8.3) + the v0.9.x capacity work + the v1.0 distribution channels (Homebrew, Docker, Helm) + integration recipes. See `docs/PLAN.md` for the current state.

## What you can still do

- **File bug reports** for clear-cut runtime defects (panic, data corruption, security issue, broken wire shape against the documented contract). These get acknowledged and triaged. Prefer a minimal reproduction.
- **File security disclosures** privately via the email in the project's repository profile, NOT in a public issue.
- **Build downstream consumers.** The wire surface (`/v1/*` HTTP, gRPC, and the embedded Web UI) is documented in `docs/`; external systems integrating against loomcycle are very welcome and tracked as production users to inform v1.0 design.
- **Fork and self-host.** The Apache-2.0 license permits all forms of use. If your fork diverges, that is an entirely supported path; we will not chase compatibility with downstream forks during v0.8 / v0.9 / v1.0 development.

## What will happen to PRs opened during the freeze

PRs opened during this period will be:

1. Acknowledged with a link to this document.
2. Closed (NOT merged) without prejudice to the underlying idea.
3. The author is welcome to re-open the same change after v1.0 ships.

If your PR is a security fix, mark it as such — those are reviewed during the freeze.

## Resuming contributions

When v1.0 ships, this document is replaced with a normal contributor guide covering:

- The architect → plan → branch → code → tests → review → merge chain (already documented for internal contributors in `CLAUDE.md`).
- The RFC process for non-trivial features (RFC under `doc-internal/rfcs/<feature>.md` before code).
- Code style, commit message conventions, test discipline, security rules.

Until then: thank you for your interest, and please respect the freeze.
