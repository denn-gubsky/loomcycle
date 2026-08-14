# Vendored Swagger UI assets

`swagger-ui.css` and `swagger-ui-bundle.js` are the official, unmodified
Swagger UI distribution, vendored so `/v1/docs` is fully self-contained (no CDN,
works air-gapped). Served by `handleOpenAPIDocs` in `../openapi.go`.

- **Package:** `swagger-ui-dist`
- **Version:** `5.17.14`
- **Source:** `https://unpkg.com/swagger-ui-dist@5.17.14/`
- **License:** Apache-2.0 (same as loomcycle)

SHA-256 (verify after any update):

```
40170f0ee859d17f92131ba707329a88a070e4f66874d11365e9a77d232f6117  swagger-ui.css
c2e4a9ef08144839ff47c14202063ecfe4e59e70a4e7154a26bd50d880c88ba1  swagger-ui-bundle.js
```

To update: download the new pinned version, replace both files, update the
version + SHA-256 above, and re-run the tests. `index.html` uses `BaseLayout`
and only these two files (no standalone preset).
