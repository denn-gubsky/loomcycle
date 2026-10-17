---
name: Crypto
description: "Crypto tool — encrypt, decrypt, sign, verify, MAC, one-time codes, hashing and random values with keys the runtime holds; key sources, scopes, formats and how to keep a decrypted value out of the stored transcript."
---
The `Crypto` tool does cryptography with **keys the runtime holds**. You name a
key; the runtime opens it, does the operation and gives you only the result.
You never see key material, and nothing you do with this tool can show it to
you.

## When to use it — and when not

- **Use it** to protect a value before you store it (`encrypt`, later
  `decrypt`), to sign or MAC data for another system (`sign`, `mac`), to check
  a signature or MAC on data you received (`verify`, `verify_mac`), to produce
  a one-time login code (`totp`), and for hashes and random values (`hash`,
  `random`).
- **Not for secrets you were told in the conversation.** Encrypting text that
  is already in this conversation does not hide it from the conversation.
- **Not for passwords.** Never hash a password to store it with `hash`.

## Keys

Name a key in exactly one of three ways:

- `key` — a **crypto key** stored with CredentialDef. Its type and versions
  are stored with it, so you pass only the name.
- `cred` — an **ordinary stored credential** used as key material, such as a
  partner's webhook secret. Pass `key_type` too, and `key_encoding` when the
  value is not written the default way for that type.
- `run` — a **credential your run's caller passed** with the run. Pass
  `key_type` too.

Which of these your run may use is its **crypto scope**: `agent`, `user`,
`tenant` (where stored keys are looked up) and `run`. `Context op=permissions`
lists them. With no scope you can still use `hash` and `random`.

A `key` name is used when exactly one of your scopes holds a crypto key of
that name. If more than one does, the call is refused until you name the one
you mean with `key_scope` (`agent`, `user` or `tenant`); a key in one scope
never stands in for another's. An envelope records its key's scope, so a
`decrypt` or `rewrap` of an envelope needs no `key_scope`.

Each key type does certain operations:

- `aes-256-gcm`, `xchacha20-poly1305` — `encrypt`, `decrypt`, `rewrap`
- `hmac-sha256`, `hmac-sha512` — `mac`, `verify_mac`, and `sign` / `verify`
  as a JWS token
- `ed25519`, `ecdsa-p256` — `sign`, `verify`, `public_key`
- `totp` — `totp`

## Formats and context

`encrypt` returns an **envelope** by default: `lcx1:<source>:<name>:<version>:…`.
It names its key (and a crypto key's scope: `kt` tenant, `ku` user, `ka`
agent), so `decrypt` needs only the ciphertext. Ask for
`format: "raw"` (base64 of nonce, ciphertext and tag) only for a system that
reads that form.

`context` ties a ciphertext to what it belongs to, such as `invoice:1234`. It
decrypts only with the same `context`. Use it whenever a value belongs to one
record or field.

## Keeping a decrypted value out of storage

`decrypt` with `sensitive: true` keeps the plaintext out of the stored
transcript; you still read it in this turn. Without it, the result is stored
like any tool result. The `plaintext` you pass to `encrypt` is never stored.

## Limits

Each data argument is capped (1 MiB by default), and a run may make a limited
number of Crypto calls (100 by default). Failures to decrypt or verify never
say why, so a wrong key, a wrong `context` and a changed byte all look alike.

## Examples

```json
{"op": "encrypt", "key": "customer-fields", "plaintext": "4111 1111 1111 1111", "context": "customer:42/card"}
```
