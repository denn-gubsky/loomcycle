# Deployment Context

Operator-authored context every agent in this tenant shares. Edit this document to
give agents stable facts about the DEPLOYMENT rather than about one person — the
things that are equally true whichever user is asking.

It is read by every agent whose prompt injects it, so keep it short and factual.
This is reference data agents read, not instructions they must obey, and anything
here is visible to every user of the tenant — do not put per-user detail or
anything you would not show all of them.

## What this deployment is for

The team or product this runtime serves, and what agents here are expected to do.
(for example: "Platform team's internal assistant; code review, incident
write-ups and release notes for the payments stack.")

## Systems and vocabulary

Names an agent will meet that it cannot infer — services, repositories,
environments, and any term this team uses in a specific way. (for example:
"`ledger` is the double-entry service, not the reporting DB. `prod-eu` is the
only tenant-facing environment.")

## Standing conventions

Durable, cross-user working rules for this deployment. (for example: "Release
notes cite the PR number. Never open a PR against `main` directly.")
