# syntax=docker/dockerfile:1
#
# Derived loomcycle image for the cloud deployment: the official distroless
# loomcycle + the PinchTab CLIENT binary.
#
# Why: PinchTab's MCP server is stdio-only (`pinchtab mcp` speaks JSON-RPC over
# stdio), so loomcycle drives the browser by SPAWNING `pinchtab --server
# http://pinchtab:9867 mcp` as a stdio MCP subprocess — which needs the pinchtab
# binary present in loomcycle's own container. Chromium stays in the separate
# `pinchtab` server sidecar; only this ~28 MB client binary (no browser) is added.
ARG LOOMCYCLE_TAG=1.38.0
ARG PINCHTAB_TAG=0.15.0

FROM pinchtab/pinchtab:${PINCHTAB_TAG} AS pinchtab

FROM denngubsky/loomcycle:${LOOMCYCLE_TAG}
# Distroless base: no shell, but a plain COPY of an already-executable binary is
# fine. /usr/local/bin is on the image PATH; the config references the full path.
COPY --from=pinchtab /usr/local/bin/pinchtab /usr/local/bin/pinchtab
