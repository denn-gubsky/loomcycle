# The `loomcycle-toolbox` image

**The default `denngubsky/loomcycle` image is distroless** — no shell, no
package manager, no Python, no compilers ([`Dockerfile`](../Dockerfile) →
`gcr.io/distroless/static:nonroot`). That is the right default: a ~40 MB image
with almost no attack surface. But it means an agent **cannot run a script or
compile code** — the `Bash` tool execs `/bin/sh`, which doesn't exist there, and
`Bashbox` (the pure-Go shell) has no real toolchain to reach.

`denngubsky/loomcycle-toolbox` is the **same loomcycle binary on a non-distroless
Debian base with a development toolchain baked in** (Python, Go, Rust, C/C++,
Node + npm, plus `git`, `gh`, `curl`). Swap the image and your agents can run
`python3`, `go build`, `cargo test`, `npm ci`, `gcc`, etc. — no loomcycle code
change, no sidecar.

**Thesis:** use the toolbox image when you want a trusted single-tenant deploy
to run code the easy way; use the distroless image (plus the builder-sidecar
sandbox) when isolation matters.

---

## ⚠️ When NOT to use this — isolation

The toolbox image runs agent code **inside loomcycle's own container**. There is
**no per-command sandbox**: a command reaches everything the loomcycle process
can — every mounted volume, the process environment, and the network. This is
the *same* trust boundary you already accept by enabling the `Bash` tool or the
Bashbox host-command fallback (see `CLAUDE.md` rule #7 / `docs/TOOLS.md`), just
with a real toolchain behind it.

That is acceptable for a **single-tenant, trusted** deployment (e.g. your own
local-agent fan-out). It is **not** safe for:

- untrusted prompts / code you would not run on your own machine,
- multi-tenant deployments where one tenant's code must not see another's data.

For those, keep the distroless image and run each code-execution session in an
isolated, ephemeral, network-off container via the **builder-sidecar sandbox**
(a separate, upcoming component — a sidecar owns podman + tmpfs + gVisor and
exposes `sandbox_*` tools; loomcycle stays distroless and drives it over MCP).

---

## What's inside

| Toolchain | Provided by |
|---|---|
| Python 3 + `pip` + `venv` | Debian packages |
| Go | official tarball at `/usr/local/go` (`GOPATH=/home/nonroot/go`) |
| Rust + Cargo | `rustup` (stable, minimal), system-wide under `/usr/local` |
| C / C++ | `build-essential` (gcc/g++/make), `clang`, `cmake`, `pkg-config` |
| Node + npm | Debian packages |
| `git`, `gh`, `curl` | for cloning, PRs, and agent HTTP/API calls |

- **User:** `nonroot`, uid/gid **65532**, home `/home/nonroot` — identical to the
  distroless image, so config (`/home/nonroot/.config/loomcycle`) and data mounts
  carry over unchanged. The image swap is drop-in.
- **Architectures:** `linux/amd64` + `linux/arm64`.
- **Size:** large (a full toolchain — expect several hundred MB), the deliberate
  cost of a real dev environment vs the ~40 MB distroless image.

---

## Using it

Two ways to give an agent the toolchain, both already in loomcycle — the toolbox
image just makes the underlying binaries exist:

1. **The `Bash` tool** — set `LOOMCYCLE_BASH_ENABLED=1` and bind the agent a
   `volumes:` working directory. `Bash` now has a real `/bin/sh` + the toolchain.
2. **Bashbox host-command fallback** — allowlist the binaries you want
   (`LOOMCYCLE_BASHBOX_ENABLED=1`,
   `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS=python3,go,cargo,npm,gcc,git,gh`,
   plus `..._FALLBACK_ALLOWED_ENV` / `..._FALLBACK_ALLOWED_CREDS` as needed).

Both require the agent to have a `volumes:` binding — that bound directory is the
compile/test workspace (see `docs/TOOLS.md` on volumes).

### Docker Compose (image swap)

Take your existing compose and change only the image tag on the loomcycle
service (everything else — env, volumes, ports — is unchanged):

```yaml
services:
  loomcycle:
    image: denngubsky/loomcycle-toolbox:latest   # was denngubsky/loomcycle:latest
    environment:
      LOOMCYCLE_LISTEN_ADDR: 0.0.0.0:8787
      LOOMCYCLE_BASH_ENABLED: "1"                 # or the Bashbox fallback vars
      # ... your existing provider keys / auth token
    volumes:
      - ./config:/home/nonroot/.config/loomcycle:ro
      - ./data:/home/nonroot/.local/share/loomcycle
      - ./work:/mnt/work                          # agent compile/test workspace
```

Then declare `/mnt/work` as a `volumes:` binding in your loomcycle config and
add it to the agents that should run code.

---

## Tags & registry

| Tag | Points at |
|---|---|
| `denngubsky/loomcycle-toolbox:latest` | most recent stable |
| `denngubsky/loomcycle-toolbox:vX.Y.Z` | exact pin (recommended for prod) |

The toolbox image is versioned in lockstep with the main image — the same `vX.Y.Z`
tag builds both. (Docker Hub strips the hyphen from `denn-gubsky` → `denngubsky`;
GHCR keeps it.)
