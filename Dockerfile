# syntax=docker/dockerfile:1.7
# SyncBridge vNext — frontend compiled to static assets, Go host-executor runtime.
FROM node:26.8.2-alpine3.24@sha256:ef24c5053d50fdc3e4e56eb4e7ddb7861874ab0fdc797046ba897581deb8e868 AS web
WORKDIR /src/webui
COPY webui/package.json webui/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --ignore-scripts --no-audit --no-fund
COPY webui/ ./
RUN mkdir -p /src/cmd/syncbridge/web && npm run build

FROM golang:1.27.1-alpine3.24@sha256:f86f1a6701e3dcc445fec097a42f78b758f15950ccf032c2d3e54e2754d32fdb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=web /src/cmd/syncbridge/web ./cmd/syncbridge/web
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /out/syncbridge ./cmd/syncbridge

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
LABEL org.opencontainers.image.source="https://github.com/GodsQuantum/SyncBridge" \
      org.opencontainers.image.description="SyncBridge host-executor controller"
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    util-linux-misc
COPY --from=build /out/syncbridge /usr/local/bin/syncbridge
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/syncbridge", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/syncbridge"]
