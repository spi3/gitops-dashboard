# syntax=docker/dockerfile:1
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG SOURCE=https://github.com/spi3/gitops-dashboard

# context-audit inspects the real, ignore-filtered build context through a
# read-only bind mount (not a COPY, so its content never enters an image
# layer or this stage's own filesystem beyond the narrow script below). It is
# defense in depth: it cannot see anything .dockerignore already excluded,
# and it cannot prevent allowed content from entering the builder context or
# BuildKit's source cache. It only proves that no local COPY/ADD in a
# downstream stage runs on content bearing a private-key marker.
FROM alpine:3.22 AS context-audit
COPY scripts/docker-context-audit.sh /usr/local/bin/docker-context-audit.sh
RUN chmod 0755 /usr/local/bin/docker-context-audit.sh
RUN --mount=type=bind,source=.,target=/context,ro \
    docker-context-audit.sh /context \
    && touch /audit-ok

FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /src
# This COPY --from=context-audit is an inter-stage copy, not a local-context
# copy: it creates an explicit graph dependency on the audit stage that
# BuildKit must resolve (and fail on, if the audit failed) before any local
# COPY below can run. Dockerfile textual order alone would not guarantee that.
COPY --from=context-audit /audit-ok /tmp/context-audit-ok
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json vite.config.ts eslint.config.js ./
COPY web ./web
RUN npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
ARG VERSION
ARG COMMIT
ARG BUILD_DATE
COPY --from=context-audit /audit-ok /tmp/context-audit-ok
RUN apk add --no-cache build-base
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/gitops-dashboard ./cmd/gitops-dashboard
COPY internal ./internal
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=1 go build -buildvcs=false \
    -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=${VERSION} -X github.com/example/gitops-dashboard/internal/version.Commit=${COMMIT} -X github.com/example/gitops-dashboard/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/gitops-dashboard ./cmd/gitops-dashboard

FROM alpine:3.22
ARG VERSION
ARG COMMIT
ARG BUILD_DATE
ARG SOURCE
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="${SOURCE}"
RUN apk add --no-cache ca-certificates git iputils libcap openssh-client setpriv \
    && addgroup -S -g 10001 gitops-dashboard \
    && adduser -S -D -u 10001 -G gitops-dashboard -h /home/gitops-dashboard gitops-dashboard \
    && mkdir -p /app /data/repos /home/gitops-dashboard \
    && chown -R gitops-dashboard:gitops-dashboard /app /data /home/gitops-dashboard \
    && setcap cap_net_raw+ep "$(command -v ping)"
ENV HOME=/tmp
WORKDIR /app
COPY --from=context-audit /audit-ok /tmp/context-audit-ok
COPY --from=build /out/gitops-dashboard /usr/local/bin/gitops-dashboard
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh
EXPOSE 8080
ENTRYPOINT ["docker-entrypoint.sh"]
