# 42W Matter sidecar — Node.js container running matter.js.
#
# Single-stage: matter.js ships with native-ish dependencies that are
# simplest to keep on the same node:alpine base for both install and
# runtime, and the sidecar's own source is tiny compared to its
# node_modules tree.

FROM node:22-alpine

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY tsconfig.json ./
COPY src/ ./src/
RUN npm ci --include=dev && npm run build && npm prune --omit=dev

VOLUME ["/data"]
EXPOSE 5540 5580

ENTRYPOINT ["node", "dist/index.js"]
