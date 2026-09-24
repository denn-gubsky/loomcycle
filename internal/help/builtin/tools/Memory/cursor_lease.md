---
name: Memory/cursor_lease
description: "Memory op=cursor_lease — take (or refresh) the consolidation lease on a memory target so no other pass works it at the same time; consolidation machinery."
---
`cursor_lease` is consolidation machinery. A pass takes the target's lease
before it does any work, so two passes never consolidate the same target at
once. **If it returns `acquired: false`, another pass holds the lease — stop
and do nothing.** Calling it again while you hold the lease refreshes it. The
lease expires on its own, so a crashed pass never blocks the target for long.
An ordinary agent does not need it. Needs the consolidation grant.

## Arguments

- `scope` (required) — the target, usually `user`.
- `lease_ttl_ms` — how long to hold the lease, in milliseconds. Default
  60000 (one minute); at most one hour. Refresh it with another call if your
  pass runs longer.

## Returns

`{acquired, owner, watermark_completed_at, watermark_session_id, leased_by,
lease_expires_at}`. `owner` is this run's identity; when `acquired` is
`false`, `leased_by` names the pass that holds it.

## Errors

- `memory: cursor_lease requires the memory_consolidation grant ...` — not a
  consolidator; retrying is pointless.
- `cursor_lease: no stable run/agent identity to own the lease`.
- `cursor_lease: ...` wraps a storage failure.

## Examples

Take the lease on this user's memory for five minutes:

```json
{"op": "cursor_lease", "scope": "user", "lease_ttl_ms": 300000}
```

```json result
{"acquired": true, "owner": "run_7f3a9c", "leased_by": "run_7f3a9c", "lease_expires_at": "2026-03-14T09:05:00Z",
 "watermark_completed_at": "2026-03-13T21:40:12.513942Z", "watermark_session_id": "b4407b522dc11495d3de371311db17f0"}
```

Someone else is already consolidating — stop:

```json
{"op": "cursor_lease", "scope": "user"}
```

```json result
{"acquired": false, "owner": "run_7f3a9c", "leased_by": "run_2b81e0", "lease_expires_at": "2026-03-14T09:01:30Z",
 "watermark_completed_at": "2026-03-13T21:40:12.513942Z", "watermark_session_id": "b4407b522dc11495d3de371311db17f0"}
```
