---
name: Context/channels
description: "Context op=channels — every declared channel with its scope, TTL, size cap and hold flag, and whether your allowlists name it for publish and subscribe."
---
`channels` lists the channels that exist on this runtime, including ones you
may not use, with each one's settings. Call it before using the `Channel`
tool, to learn a channel's **scope** (which decides whose stream you read) and
whether it is held.

## Arguments

- `prefix` — only channel names starting with this, e.g. `findings/`.

## Returns

`{channels: [...], count, publish_wildcards, subscribe_wildcards}`. Each
channel is `{name, scope, semantic?, default_ttl?, max_messages?, publisher?,
hold?, publish, subscribe}`.

The `publish` and `subscribe` booleans are true only when your allowlist
names the channel **exactly**. A wildcard such as `findings/*` is not
expanded into those booleans; it is listed in `publish_wildcards` /
`subscribe_wildcards`. So a channel showing `publish: false` may still be
publishable through a wildcard. `Channel {"op":"list_channels"}` shows the
same allowlists as raw patterns.

## Errors

None in practice.

## Examples

See every channel under `findings/`:

```json
{"op": "channels", "prefix": "findings/"}
```

```json result
{"count": 2, "publish_wildcards": ["findings/*"], "subscribe_wildcards": null, "channels": [
  {"name": "findings/alpha", "scope": "tenant", "default_ttl": 86400, "publish": false, "subscribe": true},
  {"name": "findings/beta", "scope": "tenant", "default_ttl": 86400, "publish": false, "subscribe": false}]}
```
