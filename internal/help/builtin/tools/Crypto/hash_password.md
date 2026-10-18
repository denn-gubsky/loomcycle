---
name: Crypto/hash_password
description: "Crypto op=hash_password — make an Argon2id hash of a password to store; needs no key; the password is never stored in the transcript."
---
`hash_password` turns a password into a hash you can store and later check
with `verify_password`. It needs no key and no crypto scope. Use it, never
`hash`, for anything a person logs in with: it is slow on purpose and salted,
so a stolen hash is costly to guess at.

## Arguments

- `password` (required) — the password, up to 1024 bytes. It is never stored
  in the transcript.

## Returns

`{password_hash, algorithm}`. The hash is a PHC string
(`$argon2id$v=19$m=…,t=…,p=…$salt$hash`) that names its own parameters and
salt. It is safe to store and to show; two hashes of the same password differ.

## Examples

```json
{"op": "hash_password", "password": "correct horse battery staple"}
```
