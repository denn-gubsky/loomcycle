# loomcycle sandbox SESSION image — the toolchain that runs INSIDE each sandbox
# container the builder sidecar launches. This is NOT loomcycle and NOT the
# sidecar: it is the disposable environment agent code compiles/runs in.
#
# Requirements the sidecar depends on:
#   - a shell (bash) + coreutils (`sleep`, used as the container's idle command)
#   - a non-root user at uid:gid 1000:1000 (matches SANDBOX_CONTAINER_USER)
#   - toolchain caches must work under a --read-only rootfs: the sidecar sets
#     HOME/GOCACHE/CARGO_HOME/etc. into the writable /work tmpfs, so nothing here
#     needs to write outside /work or /tmp at runtime.
#
# Build + pin (the sidecar's SANDBOX_IMAGE points at this):
#   docker build -t localhost/loomcycle-sandbox-session:latest ./session
#
# Keep it pinned by digest in production.

FROM debian:bookworm-slim

ARG TARGETARCH
ARG GO_VERSION=1.26.0

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git bash coreutils \
        python3 python3-pip python3-venv \
        build-essential clang make cmake pkg-config \
        nodejs npm; \
    apt-get clean; rm -rf /var/lib/apt/lists/*

# Go (official tarball). Arch from buildx TARGETARCH, dpkg fallback for plain builds.
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; rm /tmp/go.tgz

# Rust (rustup), system-wide + world-usable so the uid-1000 session user can use it.
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
RUN set -eux; \
    curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain stable; \
    chmod -R go+rwX /usr/local/rustup /usr/local/cargo

# The toolchains live on PATH; per-session writable caches (GOCACHE, CARGO_HOME
# override, npm cache, HOME) are injected by the sidecar into the /work tmpfs.
ENV PATH="/usr/local/go/bin:/usr/local/cargo/bin:${PATH}"

# Non-root session user at 1000:1000 (the sidecar launches --user 1000:1000).
RUN groupadd --gid 1000 sandbox && \
    useradd --uid 1000 --gid 1000 --create-home --home-dir /home/sandbox --shell /bin/bash sandbox

USER 1000:1000
WORKDIR /work
# The sidecar starts the container with `sleep infinity` and exec's commands in;
# this default is a harmless fallback if run directly.
CMD ["sleep", "infinity"]
