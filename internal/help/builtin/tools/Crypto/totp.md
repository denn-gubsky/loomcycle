---
name: Crypto/totp
description: "Crypto op=totp — produce the current time-based one-time code (as an authenticator app shows) from a TOTP seed the runtime holds, without ever seeing the seed."
---
`totp` gives the current one-time code for an account protected by an
authenticator app, from a seed the runtime holds. Use it to finish a sign-in
that asks for a code.

## Arguments

- `key`, `cred` or `run` (one is required) — a `totp` key. A seed stored as
  an ordinary credential (the base32 text an app's setup shows) is used with
  `cred` and `key_type: "totp"`.
- `digits` — code length, 6 to 8 (default 6).
- `period` — seconds each code lasts (default 30).

## Returns

`{code, valid_until}`. Enter the code before `valid_until`; if it is a few
seconds away, wait for the next code.

## Examples

```json
{"op": "totp", "cred": "billing-portal-2fa", "key_type": "totp"}
```
