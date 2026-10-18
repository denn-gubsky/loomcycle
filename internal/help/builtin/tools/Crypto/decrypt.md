---
name: Crypto/decrypt
description: "Crypto op=decrypt — open a ciphertext encrypt made; an envelope names its own key; sensitive keeps the plaintext out of the stored transcript; a failure never says why."
---
`decrypt` opens what `encrypt` returned.

## Arguments

- `ciphertext` (required) — exactly as `encrypt` returned it.
- `key`, `cred` or `run` — the key. An envelope made with a crypto key names
  its key, so you may omit all three. A ciphertext made with a `cred` or `run`
  key needs that key named again, with its `key_type`. A `raw` ciphertext
  needs the key named; `version` picks a crypto key's version (by default
  every usable version is tried).
- `context` — the same `context` it was encrypted with, if any.
- `sensitive` — `true` keeps the plaintext out of the stored transcript. You
  still read it in this turn; a later turn or a resumed run does not see it
  again.
- `output_encoding` — `text` (default when the result is UTF-8) or `base64`.

## Returns

`{plaintext, encoding, key_version}`.

A failure is always "did not decrypt": a wrong key, a wrong `context`, a
retired key version and a changed byte all look the same.

## Examples

```json
{"op": "decrypt", "ciphertext": "lcx1:kt:customer-fields:2:Qk9HVVMtRVhBTVBMRS1PTkxZ", "context": "customer:42/card", "sensitive": true}
```
