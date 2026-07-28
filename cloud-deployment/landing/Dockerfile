# Landing server image. Multi-stage: install prod deps, then a slim runtime.
# The static site (cloud-web/) is NOT baked in — it's bind-mounted at /srv so the
# marketing page can change without rebuilding this image.
FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production
COPY --from=deps /app/node_modules ./node_modules
COPY package.json server.js ./
USER node
EXPOSE 8080
CMD ["node", "server.js"]
