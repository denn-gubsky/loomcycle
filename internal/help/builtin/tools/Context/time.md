---
name: Context/time
description: "Context op=time — the current UTC time (RFC3339 and Unix milliseconds) and how long this run has been going."
---
`time` gives you a clock. Use it to compute a deadline, to build a
`deliver_at` for `Channel` publish, or to compare against a message's
`published_at`. The format matches the timestamps Channel returns, so you can
compare them directly.

## Arguments

None besides `op`.

## Returns

`{now_rfc3339, unix_ms}`, in UTC, plus `run_started_at` and `elapsed_ms` when
the run's start time is known.

## Errors

None.

## Examples

What time is it, and how long have I been running?

```json
{"op": "time"}
```

```json result
{"now_rfc3339": "2026-09-24T14:05:31.204118Z", "unix_ms": 1790258731204,
 "run_started_at": "2026-09-24T14:01:02.990511Z", "elapsed_ms": 268213}
```
