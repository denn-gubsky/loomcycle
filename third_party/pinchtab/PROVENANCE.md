# pinchtab license files

`Dockerfile.browser` and `Dockerfile.media` copy the `pinchtab` binary out of
the `pinchtab/pinchtab` image into the `loomcycle-browser` and
`loomcycle-media` images. pinchtab is
MIT-licensed, and MIT requires its copyright and permission notice to travel
with every copy, but the upstream image carries neither file. These are
vendored here so both images ship them at `/usr/share/doc/pinchtab/`.

- **Project:** `https://github.com/pinchtab/pinchtab`
- **Version:** `v0.15.2` (must match the `FROM pinchtab/pinchtab:` tag in
  `Dockerfile.browser` and `Dockerfile.media`; scripts/ci/pinchtabpin checks it)
- **License:** MIT, Copyright (c) 2026 Luigi Agosti
- **Files:** `LICENSE` and `THIRD_PARTY_LICENSES.md`, unmodified, from the
  `v0.15.2` tag (byte-identical to v0.15.0)

SHA-256 (verify after any update):

```
8e526b04f1f02203eebfd55aead0657a7ae87e0591cf8a44fbee19441aa0f116  LICENSE
852c50869da49fefe4d7fbc8b54d47b18d94e0fd8a8a957cb2c9057f9d3176af  THIRD_PARTY_LICENSES.md
```

To update: when the Dockerfiles move to a new pinchtab tag (both together), fetch both
files from that tag, replace them, and update the version + SHA-256 above.
