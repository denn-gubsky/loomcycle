---
name: Crypto/sign
description: "Crypto op=sign — sign data with an Ed25519, ECDSA P-256 or HMAC key the runtime holds; a raw base64 signature or a compact JWS token."
---
`sign` signs `data` so someone else can check it came from you.

## Arguments

- `key`, `cred` or `run` (one is required) — an `ed25519` or `ecdsa-p256`
  key, or an `hmac-sha256` / `hmac-sha512` key for a JWS token.
- `data` (required) — what to sign. Text, or base64 with
  `input_encoding: "base64"`. For a JWS it becomes the token's payload.
- `format` — `raw` (default for Ed25519 and ECDSA: a base64 signature) or
  `jws` (a compact JWS token, the only form for an HMAC key). The token's
  algorithm comes from the key: EdDSA, ES256, HS256 or HS512.

## Returns

`{signature, format, key_version}`. A verifier needs the public key: get it
with `public_key`.

## Examples

```json
{"op": "sign", "key": "report-signing", "data": "quarterly report sha256 3a7bd3e2360a3d29eea436fcfb7e44c735d117c4", "format": "raw"}
```

```json
{"op": "sign", "key": "service-token-hmac", "data": "{\"sub\":\"billing-agent\",\"exp\":1792200000}", "format": "jws"}
```
