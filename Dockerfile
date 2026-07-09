ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG SOURCE=https://github.com/spi3/gitops-dashboard

FROM node:22-alpine AS ui
WORKDIR /src
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
RUN apk add --no-cache build-base
COPY go.mod go.sum ./
RUN go mod download
COPY . .
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
RUN apk add --no-cache ca-certificates git iputils openssh-client
WORKDIR /app
COPY --from=build /out/gitops-dashboard /usr/local/bin/gitops-dashboard
EXPOSE 8080
ENTRYPOINT ["gitops-dashboard"]
