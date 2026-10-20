# loomcycle-bridge-<name> — one chat bridge (RFC EQ) as a container.
#
# One Dockerfile for every platform bridge. The build CONTEXT is the bridge's
# package directory, so the same file builds Teams, Telegram, Slack and Discord:
#
#   docker build -f deploy/bridges/Dockerfile --build-arg PORT=8443 \
#     -t denngubsky/loomcycle-bridge-telegram:dev packages/bridge-telegram
#
# The image is built from the package SOURCE at the tag, not installed from npm:
# a bridge tag publishes to npm and builds the image in the same push, and a
# fresh npm version can take minutes to become installable. The lockfile pins
# @loomcycle/bridge-core to its published version either way.
#
# Configuration is environment only (see the bridge's README). With
# BRIDGE_STORE=file:/data/<name>.json, mount a volume at /data so a restart keeps
# each chat's loomcycle session.

# ---- Stage 1: install, test, build ------------------------------------------
# The output is plain JavaScript (no bridge has a native dependency), so this
# stage runs once on the build machine's own platform, never under emulation.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm test && npm run build && npm prune --omit=dev

# ---- Stage 2: runtime --------------------------------------------------------
FROM node:22-alpine
ENV NODE_ENV=production
WORKDIR /app
COPY --from=build /src/package.json ./
COPY --from=build /src/node_modules ./node_modules
COPY --from=build /src/dist ./dist
# A writable home for BRIDGE_STORE=file:/data/..., owned by the unprivileged user.
RUN mkdir -p /data && chown node:node /data
USER node
# The bridge's default listen port (it reads PORT at start; this is metadata).
ARG PORT=8080
EXPOSE ${PORT}
ENTRYPOINT ["node", "dist/cli.js"]
