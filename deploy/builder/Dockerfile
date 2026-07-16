# loomcycle-builder — the sandbox sidecar image.
#
# Runs the sidecar (this module) on a podman-capable base so it can launch
# per-session sandbox containers. loomcycle itself stays distroless and dials
# this over HTTP-MCP; ALL container/isolation complexity lives here.
#
# ⚠️  Nested containers: running podman INSIDE this container needs a host
# runtime that supports it. Recommended: the Sysbox runtime (secure, rootless
# nested containers). Fallback: run this sidecar --privileged (accept the risk;
# common on TrueNAS which lacks Sysbox). See README.md → "Deploy".
#
# Build:
#   docker build -t denngubsky/loomcycle-builder:dev --build-arg VERSION=dev .

# ---- Stage 1: build the sidecar (stdlib only, static) -----------------------
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/loomcycle-builder .

# ---- Stage 2: runtime on podman's purpose-built base ------------------------
# quay.io/podman/stable ships podman + fuse-overlayfs + slirp4netns + the
# rootless uid/gid mapping setup — the fiddly parts of running podman in a
# container are already done here.
FROM quay.io/podman/stable:latest

# gVisor (runsc) for the strong-isolation runtime tier is OPTIONAL — uncomment
# to bake it in, then set SANDBOX_RUNTIME=runsc. Left out by default to keep the
# reference image buildable everywhere (runsc needs a compatible host kernel).
# RUN dnf -y install runsc || (curl -fsSL https://storage.googleapis.com/gvisor/releases/release/latest/$(uname -m)/runsc -o /usr/local/bin/runsc && chmod +x /usr/local/bin/runsc)

COPY --from=build /out/loomcycle-builder /usr/local/bin/loomcycle-builder

EXPOSE 9000
# SANDBOX_IMAGE (the toolchain session image) + SANDBOX_AUTH_TOKEN are required
# at runtime — see README.md. The session image is built from ./session/Dockerfile.
ENTRYPOINT ["/usr/local/bin/loomcycle-builder"]
