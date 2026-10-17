---
name: Crypto/rewrap
description: "Crypto op=rewrap — re-encrypt a crypto key's ciphertext under the key's current version without the plaintext ever reaching you; use it after a key rotation."
---
`rewrap` moves a ciphertext to its crypto key's **current** version. The
plaintext stays inside the runtime: you get only the new ciphertext. Use it
after the key was rotated, so the old version can be retired.

## Arguments

- `ciphertext` (required) — a ciphertext made with a crypto key (`key`).
  Ciphertexts made with `cred` or `run` keys have no versions to move between.
- `key` — needed only for a `raw` ciphertext; an envelope names its key.
- `context` — the same `context` it was encrypted with, if any. The new
  ciphertext keeps it.

## Returns

`{ciphertext, from_version, key_version}`. Store the new ciphertext in place
of the old one.

## Examples

```json
{"op": "rewrap", "ciphertext": "lcx1:k:customer-fields:1:Qk9HVVMtRVhBTVBMRS1PTkxZ", "context": "customer:42/card"}
```
