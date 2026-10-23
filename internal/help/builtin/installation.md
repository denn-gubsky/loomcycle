---
name: installation
description: "Install loomcycle via Homebrew, Docker, or direct tarball download, or build it from a clone."
---
Three supported install paths, plus building from a clone. Pick the
one that matches your deployment model; all ship the same single
static binary plus the v0.11.1 `init` / `doctor` first-run flow.

## 1. Homebrew (macOS + Linux)

```sh
brew install denn-gubsky/loomcycle/loomcycle
loomcycle init       # bootstrap ~/.config/loomcycle/loomcycle.yaml
loomcycle doctor     # verify env + provider keys + storage
loomcycle            # start the server on 127.0.0.1:8787
```

The tap at `denn-gubsky/homebrew-loomcycle` is auto-bumped on every
release. Run `brew upgrade` to pull the latest. For background-service
operation, `brew services start loomcycle` wraps it in `launchd`
(macOS) or `systemd` (Linux).

## 2. Docker (any platform with a Docker engine)

The image lives at `docker.io/denngubsky/loomcycle` (note: Docker Hub
strips hyphens, so `denn-gubsky` on GitHub → `denngubsky` on Hub).
Multi-arch: pull works on both `linux/amd64` and `linux/arm64`
(including Apple Silicon under Docker Desktop).

First-run flow, mounting a host config directory:

```sh
mkdir -p ./config ./data
docker run --rm -v $(pwd)/config:/home/nonroot/.config/loomcycle \
  denngubsky/loomcycle:latest init --no-interactive
# Edit ./config/loomcycle.yaml if needed

docker run -d --name loomcycle \
  -p 127.0.0.1:8787:8787 \
  -v $(pwd)/config:/home/nonroot/.config/loomcycle:ro \
  -v $(pwd)/data:/home/nonroot/.local/share/loomcycle \
  -e LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32) \
  -e ANTHROPIC_API_KEY=$YOUR_KEY \
  -e LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787 \
  denngubsky/loomcycle:latest
```

`LOOMCYCLE_AUTH_TOKEN` is required here: with no auth configured the
server refuses to listen on `0.0.0.0` (open mode would serve every route
unauthenticated). `LOOMCYCLE_ALLOW_OPEN_MODE=1` overrides that, for a
throwaway test only.

For a more declarative setup, see `docker-compose.example.yaml` in
the repo — copy it to `docker-compose.yaml` and `docker compose up
-d`. The compose file documents the Postgres upgrade path (commented
out by default).

**Image security posture:** based on `gcr.io/distroless/static:nonroot`
— ~6 MB total, no shell, no package manager, runs as uid 65532 (the
distroless `nonroot` user). `docker exec ... sh` doesn't work; use
`docker logs` for debugging.

**Tags:** `vX.Y.Z` (pinned), `latest` (most recent stable). No `vX`
or `vX.Y` floating tags during v0.11.x — too early for major-version
stability promises.

## 3. Direct tarball download

Each release attaches four platform tarballs + `SHA256SUMS` to the
GitHub release. Pick your platform:

```sh
# macOS arm64 (Apple Silicon)
curl -L -o loomcycle.tar.gz https://github.com/denn-gubsky/loomcycle/releases/latest/download/loomcycle-darwin-arm64.tar.gz
# Linux amd64
curl -L -o loomcycle.tar.gz https://github.com/denn-gubsky/loomcycle/releases/latest/download/loomcycle-linux-amd64.tar.gz

tar xzf loomcycle.tar.gz
sudo mv loomcycle /usr/local/bin/
loomcycle --version
loomcycle init
```

Verify with the checksum file:

```sh
curl -L -O https://github.com/denn-gubsky/loomcycle/releases/latest/download/SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
```

## 4. Build from a clone

For development or for platforms where the prebuilt tarballs don't
fit (FreeBSD, Windows-via-WSL, custom musl libc builds). Needs Go and
Node.js:

```sh
git clone https://github.com/denn-gubsky/loomcycle
cd loomcycle
make build-all        # builds the Web UI, then ./bin/loomcycle with it embedded
./bin/loomcycle init
```

`make build` alone skips the Web UI: `internal/webui/dist/` is not
tracked in git, so the `//go:embed` directive embeds an empty bundle
and `/ui` returns 404.

`go install github.com/denn-gubsky/loomcycle/cmd/loomcycle@<version>`
does not work. The repository is split into several Go modules wired
together with `replace` directives in the root `go.mod`, and Go
refuses to install a module at a version when its `go.mod` has them.
Homebrew, Docker and the release tarballs are the supported installs;
building from a clone is the way to build from source.

## Auto-discovery

When you run `loomcycle` without `--config`, the binary walks:

1. `./loomcycle.yaml` (current directory)
2. `$XDG_CONFIG_HOME/loomcycle/loomcycle.yaml`
3. `~/.config/loomcycle/loomcycle.yaml`

`loomcycle init` writes to (2) by default, so the bare `loomcycle`
command Just Works after `init` regardless of where you cd to.

## Verification

After installing by any method, run:

```sh
loomcycle --version       # build identifier
loomcycle doctor          # config + env + storage health checks
```

`doctor` exits 0 when everything is green; 1 on any FAIL. Operators
running in CI should script around the exit code.

## Related topics

- `getting-started` — first-run walkthrough from the agent perspective.
- `fairness` — per-user concurrency quota policy (production knob).
- `observability` — OTEL trace export setup (production knob).
- `llm-gateway` — direct LLM routing endpoint (v0.11.0).
