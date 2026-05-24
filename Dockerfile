# loomcycle — distroless multi-stage build.
#
# Layout matches `make build-all`:
#   1. Build the React SPA so internal/webui/dist/ is populated for
#      //go:embed (without this, the embedded UI is just .gitkeep).
#   2. Build the pure-Go binary (CGO_ENABLED=0 — single static binary,
#      same as the goreleaser archives).
#   3. Copy the binary into distroless/static:nonroot. ~6 MB total
#      image (~2 MB base + ~4 MB binary). No shell, no package
#      manager, no init system — minimal attack surface.
#
# Build:
#   docker build -t loomcycle:dev .
#
# Run with the config auto-discovery from v0.11.1:
#   docker run --rm -v ~/.config/loomcycle:/home/nonroot/.config/loomcycle \
#     -p 8787:8787 \
#     -e LOOMCYCLE_AUTH_TOKEN=... \
#     -e ANTHROPIC_API_KEY=... \
#     loomcycle:dev
#
# First-run flow (write the default config into the mount):
#   docker run --rm -v ~/.config/loomcycle:/home/nonroot/.config/loomcycle \
#     loomcycle:dev init --no-interactive
#
# This file is the source of truth for both `docker build` from a
# checkout AND goreleaser's `dockers:` stage in .goreleaser.yaml —
# goreleaser uses a different (binary-prebuilt) flow per the comment
# in that file, but both produce identical runtime images.

# ---- Stage 1: build the web UI ----------------------------------------------
FROM node:20-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Stage 2: build the Go binary -------------------------------------------
FROM golang:1.26-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Bring in the freshly-built web/dist via the symlink the embed walks.
COPY . .
COPY --from=web-builder /src/internal/webui/dist/ ./internal/webui/dist/
# Pull the build-version stamp from a VCS-stamped build (same flags as
# goreleaser when building from a tag).
ARG BUILD_VERSION=docker
ARG BUILD_COMMIT=docker
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X main.buildVersion=${BUILD_VERSION} \
      -X main.buildCommit=${BUILD_COMMIT} \
      -X main.buildTime=${BUILD_TIME}" \
    -o /out/loomcycle ./cmd/loomcycle

# ---- Stage 3: runtime image -------------------------------------------------
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=go-builder /out/loomcycle /usr/local/bin/loomcycle
# Distroless runs as user "nonroot" (uid 65532) by default — operator
# mounts should match that uid for write access to the config dir.
USER nonroot
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/loomcycle"]
