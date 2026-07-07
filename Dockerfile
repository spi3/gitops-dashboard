FROM node:22-alpine AS ui
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json vite.config.ts eslint.config.js ./
COPY web ./web
RUN npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
RUN apk add --no-cache build-base
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=1 go build -buildvcs=false -o /out/gitops-dashboard ./cmd/gitops-dashboard

FROM alpine:3.22
RUN apk add --no-cache ca-certificates git iputils openssh-client
WORKDIR /app
COPY --from=build /out/gitops-dashboard /usr/local/bin/gitops-dashboard
EXPOSE 8080
ENTRYPOINT ["gitops-dashboard"]
