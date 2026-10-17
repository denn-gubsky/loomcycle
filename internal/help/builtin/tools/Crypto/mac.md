---
name: Crypto/mac
description: "Crypto op=mac — compute an HMAC-SHA256 or HMAC-SHA512 over data with a key the runtime holds, e.g. to sign a webhook body for another system."
---
`mac` computes an HMAC over `data`, such as the signature header another
system expects on a webhook you send it.

## Arguments

- `key`, `cred` or `run` (one is required) — an `hmac-sha256` or
  `hmac-sha512` key. A partner's shared secret stored as an ordinary
  credential is used with `cred` and `key_type`.
- `data` (required) — the exact bytes to MAC. Text, or base64 with
  `input_encoding: "base64"`. A single changed byte, such as a re-serialized
  JSON body, gives a different MAC.
- `output_encoding` — `hex` (default) or `base64`.

## Returns

`{mac, key_version}`.

## Examples

```json
{"op": "mac", "cred": "partner-webhook-secret", "key_type": "hmac-sha256", "key_encoding": "raw", "data": "{\"event\":\"paid\",\"id\":981}"}
```
