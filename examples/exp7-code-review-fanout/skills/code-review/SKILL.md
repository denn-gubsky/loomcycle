---
name: code-review
description: How to review a slice of a code repository with high precision — confidence-scored findings, project-guideline compliance, and Karpathy behavioral guidelines (simplicity, surgical scope, surfaced assumptions). Use when reviewing or auditing source files.
license: MIT
tools: [Read, Grep, Glob]
---

# Code Review

Synthesized for loomcycle exp7 from Claude Code's `code-reviewer` agent + `code-review` command +
the `karpathy-guidelines` skill. Methodology for reviewing a slice of a repository with high
precision and minimal false positives.

## What to look for
- **Project-guideline compliance.** Honor the repo's own rules (e.g. `CLAUDE.md`): import patterns,
  error handling, logging, naming, platform compatibility, testing conventions.
- **Bugs that bite in practice.** Logic errors, nil/undefined handling, race conditions, resource
  leaks, unchecked errors, security issues (injection, secret handling, authz gaps), performance.
- **Quality.** Significant duplication, missing critical error handling, inadequate test coverage.

## Confidence scoring (the precision lever)
Rate each candidate issue 0–100:
- **0** false positive / pre-existing; **25** maybe; **50** real but minor/nit; **75** verified, will
  hit in practice / guideline-backed; **100** certain, frequent.
- **Only report issues with confidence ≥ 80.** Quality over quantity — a short list of true issues
  beats a long list of nits.

## Karpathy behavioral guidelines (apply to the review itself)
- **Surface assumptions.** If a finding depends on an assumption (caller behavior, invariant), say
  so. If unsure, lower the confidence rather than over-claim.
- **Simplicity.** Flag overcomplication: speculative abstractions, unrequested configurability,
  error handling for impossible cases, 200 lines that could be 50.
- **Surgical scope.** Judge whether changes touch only what they must; flag drive-by edits/refactors
  and orphaned (newly-unused) imports/vars. Don't demand rewrites of code that isn't broken.
- **Verifiable.** Prefer findings a developer can act on + verify (a failing case, a concrete fix).

## Output per issue
`file:line` · severity (Critical | Important) · confidence · one-line problem · concrete fix.
Group by severity. If no ≥80 issues in the slice, say so with a one-line "meets standards" summary.
