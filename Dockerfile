# syntax=docker/dockerfile:1.7

# 1.27.4 or later: GO-2026-6859 (net/http).
FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:ce59b41415e2524ae99926fe1f37ce48ba8c194dcbd2a56a65436cdcefa2bd34 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS mayflyd
COPY --from=build /out/mayflyd /mayflyd
USER 65532:65532
ENTRYPOINT ["/mayflyd"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS llmstub
COPY --from=build /out/llmstub /llmstub
USER 65532:65532
ENTRYPOINT ["/llmstub"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS mcpstub
COPY --from=build /out/mcpstub /mcpstub
USER 65532:65532
ENTRYPOINT ["/mcpstub"]

# The agent image includes Node, npm/npx, git, curl, jq, and ripgrep so stdio
# MCP servers can be installed or run in the session. The runtime filesystem is
# read-only; HOME and caches point at the writable /tmp mount.
#
# The base digest is the current index for node:26-bookworm-slim and still
# ships libpam 1.5.2-6+deb12u2 (CVE-2025-6020). The fixed 1.5.2-6+deb12u3 is in
# bookworm-security, released after the base image, so upgrade every installed
# package after the install. Without the upgrade the release scan fails on the
# four libpam packages. Keep --no-install-recommends and drop the apt lists in
# the same layer.
FROM node:26-bookworm-slim@sha256:0042e17d232a87a2cd485be1da7900fca4d10a4ad11b29e9285da1d2e54798c9 AS mayfly-agent
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl jq ripgrep \
 && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -o Dpkg::Options::=--force-confold \
 && rm -rf /var/lib/apt/lists/*
# The npm bundled with the base image can lag behind fixes in its own
# dependencies (undici, brace-expansion); the release scan fails on those.
RUN npm install -g npm@12.2.3 \
 && npm cache clean --force \
 && rm -rf /root/.npm
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
