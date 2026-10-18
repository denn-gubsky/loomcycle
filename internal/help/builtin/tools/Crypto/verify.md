---
name: Crypto/verify
description: "Crypto op=verify — check a signature or a JWS token with a key the runtime holds or a PEM / JWK public key you were given; returns valid true or false."
---
`verify` checks a signature on data, or a compact JWS token.

## Arguments

- `signature` (required) — a raw base64 signature, or a JWS token. For a token
  leave `data` empty: the payload is inside it and is returned.
- `data` — the signed data, for a raw signature.
- `from` — read the data from a store instead: `volume:<path>`,
  `memory:<scope>/<key>` or `document:<scope>/<chunk_id>`.
- One of:
  - `key`, `cred` or `run` — a key the runtime holds. For a signing key its
    public half is used; every usable version of a crypto key is tried.
  - `public_key` — a PEM or JWK Ed25519, P-256 or RSA public key (an RSA key
    verifies RSA-PSS / PS256), for a signer whose key this deployment does not
    hold.

## Returns

`{valid}`, plus `key_version` for a held key, and `kid` and `payload` for a
JWS token. `valid: false` never says why.

## Examples

```json
{"op": "verify", "public_key": "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE=\n-----END PUBLIC KEY-----", "data": "release v3.2.0 manifest", "signature": "c2lnbmF0dXJlLWV4YW1wbGU="}
```
