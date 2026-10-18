---
name: Crypto/encrypt
description: "Crypto op=encrypt — encrypt with a key the runtime holds, or to someone else's public key; returns an envelope, raw base64 or a JWE; from / to keep the data away from you."
---
`encrypt` seals data. The runtime picks a fresh nonce every time, so two
encryptions of the same text differ. It works three ways:

- with a held symmetric key (`aes-256-gcm`, `xchacha20-poly1305`): an envelope
  by default, or `raw`, or (`aes-256-gcm` only) `jwe`;
- with a held recipient key (`x25519`, `rsa-oaep-*`): a JWE to that key's own
  public half, which only its holder decrypts;
- with `public_key`: a JWE to someone else's X25519 or RSA public key. This
  holds no key, so it needs no crypto scope.

## Arguments

- `key`, `cred` or `run` — the key. With `cred` or `run`, also `key_type` and,
  if needed, `key_encoding` or `derive`. Or `public_key` instead of all three.
- `plaintext` — what to encrypt. Text, or base64 bytes with
  `input_encoding: "base64"`. It is never stored in the transcript.
- `from` — read the plaintext from a store instead: `volume:<path>`,
  `memory:<scope>/<key>` or `document:<scope>/<chunk_id>`. Give `plaintext` or
  `from`, not both.
- `to` — write the ciphertext to a store instead of returning it.
- `input_encoding` — `text` (default) or `base64`.
- `context` — associated data. The ciphertext decrypts only with the same
  `context`. Use it to tie the value to the record or field it belongs to. A
  JWE carries none, so `context` is refused with it.
- `format` — `envelope` (default for symmetric keys), `raw` or `jwe`.

## Returns

`{ciphertext, key_version}`, plus `format: "jwe"` for a JWE. An envelope is
`lcx1:<source>:<name>:<version>:…` and names its key, so `decrypt` needs only
the ciphertext and the `context`. A `raw` ciphertext and a JWE need the key
named again to decrypt. With `to`, the result is `{to, bytes, key_version}`
and holds no ciphertext.

## Examples

```json
{"op": "encrypt", "key": "customer-fields", "plaintext": "4111 1111 1111 1111", "context": "customer:42/card"}
```

```json
{"op": "encrypt", "public_key": "{\"kty\":\"OKP\",\"crv\":\"X25519\",\"x\":\"hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo\"}", "plaintext": "for the partner's eyes only"}
```

```json
{"op": "encrypt", "key": "archive-key", "from": "volume:reports/q3.csv", "to": "volume:reports/q3.csv.lcx"}
```
