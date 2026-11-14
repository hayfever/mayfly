# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0
ARG COMMIT=none
ARG DATE=unknown
RUN LDFLAGS="-s -w -X github.com/hayfever/mayfly/internal/version.Version=${VERSION} -X github.com/hayfever/mayfly/internal/version.Commit=${COMMIT} -X github.com/hayfever/mayfly/internal/version.Date=${DATE}" \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mayflyd ./cmd/mayflyd \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mayfly-agent ./cmd/mayfly-agent \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mayfly ./cmd/mayfly \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mayfly-harness ./cmd/mayfly-harness \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mayfly-plugin-shim ./cmd/mayfly-plugin-shim \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/llmstub ./cmd/llmstub \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/mcpstub ./cmd/mcpstub

FROM gcr.io/distroless/static-debian12:nonroot AS mayflyd
COPY --from=build /out/mayflyd /mayflyd
USER 65532:65532
ENTRYPOINT ["/mayflyd"]

FROM gcr.io/distroless/static-debian12:nonroot AS llmstub
COPY --from=build /out/llmstub /llmstub
USER 65532:65532
ENTRYPOINT ["/llmstub"]

FROM gcr.io/distroless/static-debian12:nonroot AS mcpstub
COPY --from=build /out/mcpstub /mcpstub
USER 65532:65532
ENTRYPOINT ["/mcpstub"]

# The agent image includes Node, npm/npx, git, curl, jq, and ripgrep so stdio
# MCP servers can be installed or run in the session. The runtime filesystem is
# read-only; HOME and caches point at the writable /tmp mount.
FROM node:26-bookworm-slim AS mayfly-agent
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl jq ripgrep \
 && rm -rf /var/lib/apt/lists/*
RUN groupadd --gid 65532 mayfly \
 && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin mayfly
COPY --from=build /out/mayfly-agent /usr/local/bin/mayfly-agent
COPY --from=build /out/mayfly-harness /usr/local/bin/mayfly-harness
COPY --from=build /out/mayfly-plugin-shim /usr/local/bin/mayfly-plugin-shim
RUN mkdir -p /opt/mayfly/harnesses /opt/mayfly/plugins
ENV HOME=/tmp \
    XDG_CACHE_HOME=/tmp/cache \
    NPM_CONFIG_CACHE=/tmp/npm
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/mayfly-harness"]
