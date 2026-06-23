# loomcycle on TrueNAS SCALE

Run loomcycle as a TrueNAS SCALE application (Electric Eel 24.10+, the Docker
Compose app engine). **RFC AR.** Two routes — both documented in
[`../../docs/TRUENAS.md`](../../docs/TRUENAS.md):

| File | Route | When |
|---|---|---|
| [`docker-compose.yaml`](docker-compose.yaml) | **Custom app** (Apps → Discover → ⋮ → *Install via YAML*, paste) | Fastest. Works today; you edit the compose directly. |
| [`catalog/`](catalog/) | **Catalog app** (install wizard via `questions.yaml`) | Formal packaging — an install/edit form, published to a train. |
| [`loomcycle.overlay.example.yaml`](loomcycle.overlay.example.yaml) | the thin overlay (agent `volumes:` block) both routes mount | Copy to your config dataset. |

Both routes share the same shape: the binary's **embedded presets** (RFC AQ)
supply the provider/tier matrix (`LOOMCYCLE_PRESETS`); a thin overlay supplies the
agent **Volumes** (RFC AH) mapped to ZFS datasets; **secrets** are TrueNAS-managed
env (never a yaml layer); storage is your **existing Postgres 16** (the app bundles
no DB). Ingress/TLS is out of scope — the container binds `0.0.0.0:8787`; front it
with your existing tunnel/proxy.

> **Catalog app caveat.** `catalog/templates/docker-compose.yaml` uses the TrueNAS
> Jinja2 + `ix-lib` render library, which TrueNAS's catalog CI vendors at build
> time (it is **not** committed here). Render-validate the catalog app on your
> TrueNAS version before publishing (RFC AR open question — schema-dialect drift).
> The **custom-app `docker-compose.yaml` is plain compose** and needs no library —
> start there.

Start with [`../../docs/TRUENAS.md`](../../docs/TRUENAS.md).
