---
name: Crypto/random
description: "Crypto op=random — cryptographically secure random bytes, a URL-safe token or a UUID, needing no key."
---
`random` makes values from the operating system's secure random source. It
needs no key and no crypto scope. Use it for tokens, identifiers and nonces
another system asks you to supply.

## Arguments

- `kind` — `token` (default: URL-safe base64, no padding), `bytes` or `uuid`
  (a version 4 UUID).
- `length` — random bytes for `token` and `bytes`, 1 to 4096 (default 32).
- `output_encoding` — for `bytes`: `hex` (default) or `base64`.

## Returns

`{kind, value}`.

## Examples

```json
{"op": "random", "kind": "token", "length": 24}
```
