---
name: Crypto/public_key
description: "Crypto op=public_key — get the PEM and JWK public key of an Ed25519 or ECDSA P-256 key the runtime holds, to hand to whoever verifies your signatures."
---
`public_key` returns the public half of a signing key. It is not secret: give
it to whoever must verify what you sign.

## Arguments

- `key`, `cred` or `run` (one is required) — an `ed25519` or `ecdsa-p256`
  key.
- `version` — a crypto key's version (default: the current one).

## Returns

`{pem, jwk, key_version}`. The JWK's `kid` matches the `kid` in tokens the key
signs.

## Examples

```json
{"op": "public_key", "key": "report-signing"}
```
