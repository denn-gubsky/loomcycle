# syntax=docker/dockerfile:1
# Hosted loomboard browser app for loomcycle.cloud. Clones loomboard, builds the
# standalone (proxy-mode) SPA with the connect-handoff origin allowlist baked in,
# then serves it with a tiny dependency-free Node server (serve.mjs) that also
# reverse-proxies /v1/* to the loomcycle runtime (same-origin — loomcycle emits no
# CORS). The upstream is PINNED, so the app's x-loomcycle-target header is ignored
# and this is never an open relay.
#
# The serving stage reuses the node:22 build image so hosting needs NO extra
# registry pull (Docker Hub anonymous pulls rate-limit this host; caddy:2-alpine
# 429'd). Bump LOOMBOARD_REF to a tag / `main` once the connect-handoff PR merges.
ARG LOOMBOARD_REF=feature-connect-handoff
ARG CONNECT_ORIGINS=https://loomcycle.cloud

FROM node:22 AS build
ARG LOOMBOARD_REF
ARG CONNECT_ORIGINS
WORKDIR /app
RUN git clone --depth 1 --branch "${LOOMBOARD_REF}" \
      https://github.com/denn-gubsky/loomboard.git .
RUN npm ci
RUN VITE_LOOMBOARD_CONNECT_ORIGINS="${CONNECT_ORIGINS}" npm run build:standalone

FROM node:22-alpine
COPY --from=build /app/cli/public /srv
COPY serve.mjs /serve.mjs
ENV PORT=8080 SRV_ROOT=/srv LOOMCYCLE_UPSTREAM=http://tailscale:8787
EXPOSE 8080
CMD ["node", "/serve.mjs"]
