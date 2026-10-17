---
name: Crypto/encrypt
description: "Crypto op=encrypt — encrypt text or bytes with an AEAD key the runtime holds; returns a self-describing envelope (or raw base64); bind it to a record with context."
---
`encrypt` seals data with an AEAD key (`aes-256-gcm` or
`xchacha20-poly1305`). The runtime picks a fresh nonce every time, so two
encryptions of the same text differ.

## Arguments

- `key`, `cred` or `run` (one is required) — the key. With `cred` or `run`,
  also `key_type` and, if needed, `key_encoding` or `derive`.
- `plaintext` (required) — what to encrypt. Text, or base64 bytes with
  `input_encoding: "base64"`. It is never stored in the transcript.
- `input_encoding` — `text` (default) or `base64`.
- `context` — associated data. The ciphertext decrypts only with the same
  `context`. Use it to tie the value to the record or field it belongs to.
- `format` — `envelope` (default) or `raw`.

## Returns

`{ciphertext, key_version}`. An envelope is `lcx1:<source>:<name>:<version>:…`
and names its key, so `decrypt` needs only the ciphertext and the `context`.
A `raw` ciphertext is base64 of nonce, ciphertext and tag; decrypting it needs
the key named again.

## Examples

```json
{"op": "encrypt", "key": "customer-fields", "plaintext": "4111 1111 1111 1111", "context": "customer:42/card"}
```

```json
{"op": "encrypt", "run": "session-key", "key_type": "aes-256-gcm", "plaintext": "hand-off note for step 3"}
```
