---
name: Crypto/hash
description: "Crypto op=hash — SHA-256, SHA-512, SHA3-256 or SHA3-512 digest of data, needing no key; for integrity checks and content fingerprints, never for storing passwords."
---
`hash` computes a digest of `data`. It needs no key and no crypto scope.

## Arguments

- `data` (required) — what to hash. Text, or base64 with
  `input_encoding: "base64"`.
- `from` — read the data from a store instead: `volume:<path>`,
  `memory:<scope>/<key>` or `document:<scope>/<chunk_id>`.
- `algorithm` — `sha256` (default), `sha512`, `sha3-256` or `sha3-512`.
- `output_encoding` — `hex` (default) or `base64`.

## Returns

`{algorithm, digest}`.

A hash is not a secret and not a password store: anyone can hash a guess and
compare.

## Examples

```json
{"op": "hash", "data": "release v3.2.0 manifest", "algorithm": "sha256"}
```
