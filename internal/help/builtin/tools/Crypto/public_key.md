---
name: Crypto/public_key
description: "Crypto op=public_key — get the PEM and JWK public key of an asymmetric key the runtime holds: give it to whoever verifies your signatures or encrypts to you."
---
`public_key` returns the public half of an asymmetric key. It is not secret:
give it to whoever must verify what you sign (`ed25519`, `ecdsa-p256`,
`rsa-pss-*`), or to whoever should encrypt to you (`x25519`, `rsa-oaep-*`).

## Arguments

- `key`, `cred` or `run` (one is required) — an asymmetric key.
- `version` — a crypto key's version (default: the current one).

## Returns

`{pem, jwk, key_version}`. The JWK's `kid` matches the `kid` in tokens the key
signs or JWEs made to it; for an RSA or X25519 key the JWK also says its `use`
and `alg`.

## Examples

```json
{"op": "public_key", "key": "report-signing"}
```
