# Launch plan

A small plain-Markdown file to try the Document Assistant on. Paste it into the
Assistant and say: *"import this as a new document under /docs/launch and split
it into sections."* The agent will create a chunk hierarchy from the headings.

## Goals

Ship v1 to the waitlist by end of quarter. Keep the rollout reversible — a
feature flag gates the new flow so we can dark-launch to 5% first.

## Risks

- Provider rate limits during the announcement spike.
- The migration backfill is O(n) over the user table; run it off-peak.

## Runbook

1. Flip the flag to 5%.
2. Watch error rate + p99 for 30 minutes.
3. Ramp to 50%, then 100% if clean.
4. Rollback = flip the flag off; no schema change to revert.
