# Build stage: static bundle. No VITE_DEV_TOKEN is ever passed here, and the
# build refuses to run if it is set.
FROM node:22.12.0-alpine@sha256:51eff88af6dff26f59316b6e356188ffa2c422bd3c3b76f2556a2e7e89d080bd AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build && node scripts/check-dist.mjs

# Serve stage: nginx, static files, SPA fallback, strict CSP.
# Both bases are pinned to the tag AND the multi-arch digest.
FROM nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10
# API_ORIGIN is the browser-facing origin of the dev-ops API, e.g.
# https://api.example.com. Leave it empty when the API is served from the same
# origin (nginx proxy or CapRover path routing).
ENV API_ORIGIN=""
# FILES_ORIGIN is the browser-facing origin of the attachment store (silo),
# e.g. https://files.example.com. It goes into the CSP connect-src,
# img-src and media-src. Empty: attachments that live on another origin are blocked.
ENV FILES_ORIGIN=""
COPY deploy/nginx.conf.template /etc/nginx/templates/default.conf.template
COPY deploy/security.inc.template /etc/nginx/templates/security.inc.template
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
