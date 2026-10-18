---
name: Crypto/verify_mac
description: "Crypto op=verify_mac — check an HMAC another system sent with data you received, in constant time; returns valid true or false."
---
`verify_mac` checks a MAC that came with data you received, such as a
partner's webhook signature header.

## Arguments

- `key`, `cred` or `run` (one is required) — the `hmac-sha256` or
  `hmac-sha512` key it was made with.
- `data` (required) — the exact bytes that were MACed, as received.
- `mac` (required) — the MAC to check, hex or base64. Strip any prefix such
  as `sha256=` first.

## Returns

`{valid, key_version}`. `valid: false` never says why.

## Examples

```json
{"op": "verify_mac", "cred": "partner-webhook-secret", "key_type": "hmac-sha256", "key_encoding": "raw", "data": "{\"event\":\"paid\",\"id\":981}", "mac": "9f2c1e0a7b4d"}
```
