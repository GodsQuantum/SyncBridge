# syntax=docker/dockerfile:1.7
# SyncBridge vNext — host executor controller only.
# Sync engines and administrative tools execute on the host through nsenter;
# they deliberately do not live in this image.
FROM golang:1.26.7-alpine3.24@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
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
