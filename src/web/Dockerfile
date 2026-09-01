FROM node:24.15.0-alpine@sha256:d1b3b4da11eefd5941e7f0b9cf17783fc99d9c6fc34884a665f40a06dbdfc94f AS build

WORKDIR /workspace/src/web
COPY src/web/package.json src/web/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY src/web/index.html src/web/tsconfig.json src/web/tsconfig.app.json src/web/tsconfig.node.json src/web/vite.config.ts src/web/playwright.config.ts ./
COPY logo.jpg /workspace/logo.jpg
COPY src/web/src ./src
RUN pnpm run build

FROM nginx:1.31.4-alpine@sha256:db35bfc6b2951e7f8a72db5db120288c127ffaeeb4a6d4b95a26fead017d5913

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="mail-suite web" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="https://github.com/snowflake-yezi/mail-suite"

ENV MAIL_SUITE_API_UPSTREAM=http://api:8080
COPY src/web/nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /workspace/src/web/dist /usr/share/nginx/html
RUN chown -R nginx:nginx /etc/nginx/conf.d /run /var/cache/nginx /usr/share/nginx/html

USER nginx
EXPOSE 8080
