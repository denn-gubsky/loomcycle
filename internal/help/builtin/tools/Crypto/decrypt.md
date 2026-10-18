---
name: Crypto/decrypt
description: "Crypto op=decrypt — open a ciphertext encrypt made (envelope, raw or JWE); an envelope names its own key; to writes the plaintext where you never see it; a failure never says why."
---
`decrypt` opens what `encrypt` returned: an envelope, a raw ciphertext or a
JWE.

## Arguments

- `ciphertext` — exactly as `encrypt` returned it.
- `from` — read the ciphertext from a store instead: `volume:<path>`,
  `memory:<scope>/<key>` or `document:<scope>/<chunk_id>`. Give `ciphertext`
  or `from`, not both.
- `key`, `cred` or `run` — the key. An envelope made with a crypto key names
  its key, so you may omit all three. A ciphertext made with a `cred` or `run`
  key needs that key named again, with its `key_type`. A `raw` ciphertext or a
  JWE needs the key named; `version` picks a crypto key's version (by default
  a JWE's own key id decides, and a raw ciphertext tries every usable
  version).
- `context` — the same `context` it was encrypted with, if any. Not with a
  JWE.
- `to` — write the plaintext to a store instead of returning it, so it never
  passes through you. The result is then `{to, bytes, key_version}`.
- `sensitive` — `true` keeps the plaintext out of the stored transcript. You
  still read it in this turn; a later turn or a resumed run does not see it
  again.
- `output_encoding` — `text` (default when the result is UTF-8) or `base64`.

## Returns

`{plaintext, encoding, key_version}`, or with `to` `{to, bytes, key_version}`.

A failure is always "did not decrypt": a wrong key, a wrong `context`, a
retired key version and a changed byte all look the same.

## Examples

```json
{"op": "decrypt", "ciphertext": "lcx1:kt:customer-fields:2:Qk9HVVMtRVhBTVBMRS1PTkxZ", "context": "customer:42/card", "sensitive": true}
```

```json
{"op": "decrypt", "key": "archive-key", "from": "volume:reports/q3.csv.lcx", "to": "volume:reports/q3.csv"}
```
