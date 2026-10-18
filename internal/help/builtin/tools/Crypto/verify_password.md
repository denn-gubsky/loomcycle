---
name: Crypto/verify_password
description: "Crypto op=verify_password — check a password against a hash hash_password made; constant-time; needs no key."
---
`verify_password` checks a password against a stored hash from
`hash_password`. It needs no key and no crypto scope.

## Arguments

- `password` (required) — the password to check. It is never stored in the
  transcript.
- `password_hash` (required) — the stored hash, exactly as `hash_password`
  returned it.

## Returns

`{valid}`. A wrong password is `valid: false`. A hash that is not an Argon2id
PHC string, or that asks for more memory or time than the runtime allows, is
refused: something is wrong with what was stored.

## Examples

```json
{"op": "verify_password", "password": "correct horse battery staple", "password_hash": "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"}
```
