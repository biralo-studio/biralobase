# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.4
ARG NODE_VERSION=26.13.1
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS compile
ARG TARGETOS=linux
ARG TARGETARCH
ARG RELEASE_VERSION=0.1.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -p 2 -trimpath \
    -ldflags="-s -w -X github.com/biralo-studio/biralobase/internal/runtime.Version=$RELEASE_VERSION" \
    -o /out/biralobase ./cmd/biralobase

# Install the exact official Node release, including when its Docker tag has
# not yet been published. Verify the archive against the official checksum.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS node-download
ARG NODE_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "$TARGETARCH" in amd64) node_arch=x64 ;; arm64) node_arch=arm64 ;; *) exit 1 ;; esac; \
    archive="node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"; \
    curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/$archive"; \
    curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt"; \
    awk -v file="$archive" '$2 == file {print}' SHASUMS256.txt | sha256sum --check --strict; \
    tar -xJf "$archive" -C /usr/local --strip-components=1; \
    rm "$archive" SHASUMS256.txt

FROM debian:bookworm-slim AS runtime
LABEL org.opencontainers.image.title="Biralobase"
LABEL org.opencontainers.image.source="https://github.com/biralo-studio/biralobase"
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libatomic1 libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 biralobase && useradd --uid 10001 --gid 10001 --no-create-home biralobase \
    && mkdir -p /var/lib/biralobase /app && chown 10001:10001 /var/lib/biralobase
COPY --from=node-download /usr/local /usr/local
COPY --from=compile /out/biralobase /usr/local/bin/biralobase
ENV BIRALOBASE_DATA_DIR=/var/lib/biralobase BIRALOBASE_APP_DIR=/app BIRALOBASE_HTTP=0.0.0.0:8090 \
    TURSO_GO_CACHE_DIR=/tmp/turso HOME=/tmp
USER 10001:10001
WORKDIR /app
EXPOSE 8090
HEALTHCHECK --interval=10s --timeout=4s --start-period=60s CMD ["biralobase","healthcheck"]
ENTRYPOINT ["biralobase"]
CMD ["serve"]

# Opt-in, reproducible stability runner; contains no application data or secrets.
FROM compile AS qualification-compile
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go test -p 2 -c \
    -o /out/storage.test ./storage/turso

FROM runtime AS qualification
COPY --from=qualification-compile /out/storage.test /usr/local/bin/storage.test
HEALTHCHECK NONE
ENTRYPOINT ["storage.test"]
CMD ["-test.v", "-test.timeout=25h", "-test.run=TestStorageSoak|TestCrashRecovery"]

# Runs on the target architecture so npm native modules match the runtime ABI.
FROM runtime AS builder
USER root
RUN apt-get update && apt-get install -y --no-install-recommends build-essential python3 git pkg-config \
    && rm -rf /var/lib/apt/lists/*
ENV HOME=/tmp
WORKDIR /workspace
HEALTHCHECK NONE
ENTRYPOINT []
CMD ["bash"]
