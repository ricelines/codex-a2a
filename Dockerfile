# syntax=docker/dockerfile:1.7

ARG CODEX_VERSION=latest
ARG RUNTIME_IMAGE=debian:trixie-slim

FROM --platform=$BUILDPLATFORM golang:1.26.1-trixie AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /out/codex-a2a ./cmd/codex-a2a


FROM --platform=$BUILDPLATFORM alpine:3.20 AS codex_cli

ARG TARGETARCH
ARG CODEX_VERSION

RUN --mount=type=cache,target=/var/cache/apk,id=apk-cache,sharing=locked \
    apk add --no-cache ca-certificates curl tar

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) asset="codex-x86_64-unknown-linux-musl.tar.gz" ;; \
      arm64) asset="codex-aarch64-unknown-linux-musl.tar.gz" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    if [ "${CODEX_VERSION}" = "latest" ]; then \
      url="https://github.com/openai/codex/releases/latest/download/${asset}"; \
    else \
      url="https://github.com/openai/codex/releases/download/${CODEX_VERSION}/${asset}"; \
    fi; \
    curl -fsSL "${url}" -o /tmp/codex.tar.gz; \
    tar -xzf /tmp/codex.tar.gz -C /tmp; \
    bin="$(find /tmp -maxdepth 1 -type f -name 'codex-*' -print -quit)"; \
    test -n "${bin}"; \
    mv "${bin}" /codex; \
    chmod 0755 /codex


FROM ${RUNTIME_IMAGE} AS runtime

ARG CODEX_UID=10001
ARG CODEX_GID=10001

RUN --mount=type=cache,target=/var/cache/apt,id=apt-cache-trixie,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,id=apt-lists-trixie,sharing=locked \
    --mount=type=cache,target=/var/cache/apk,id=apk-cache,sharing=locked \
    set -eux; \
    if command -v apt-get >/dev/null 2>&1; then \
      apt-get update; \
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        util-linux; \
      rm -rf /var/lib/apt/lists/*; \
    elif command -v apk >/dev/null 2>&1; then \
      apk add --no-cache \
        ca-certificates \
        su-exec; \
    else \
      echo "unsupported runtime image: expected apt-get or apk" >&2; \
      exit 1; \
    fi

RUN set -eux; \
    if command -v groupadd >/dev/null 2>&1; then \
      groupadd -g "${CODEX_GID}" codex; \
    elif command -v addgroup >/dev/null 2>&1; then \
      addgroup -g "${CODEX_GID}" -S codex; \
    else \
      echo "no supported group creation tool found" >&2; \
      exit 1; \
    fi; \
    if command -v useradd >/dev/null 2>&1; then \
      useradd -m -u "${CODEX_UID}" -g "${CODEX_GID}" -s /bin/sh codex; \
    elif command -v adduser >/dev/null 2>&1; then \
      adduser -D -u "${CODEX_UID}" -G codex -h /home/codex -s /bin/sh codex; \
    else \
      echo "no supported user creation tool found" >&2; \
      exit 1; \
    fi; \
    mkdir -p /workspace /home/codex/.codex /home/codex/.codex-runtime; \
    chown "${CODEX_UID}:${CODEX_GID}" /workspace /home/codex /home/codex/.codex /home/codex/.codex-runtime; \
    chmod 0755 /workspace /home/codex /home/codex/.codex; \
    chmod 0700 /home/codex/.codex-runtime

ENV HOME=/home/codex \
    CODEX_SOURCE_HOME=/home/codex/.codex \
    CODEX_RUNTIME_HOME=/home/codex/.codex-runtime

WORKDIR /workspace

COPY --from=build /out/codex-a2a /usr/local/bin/codex-a2a
COPY --from=codex_cli /codex /usr/local/bin/codex
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 0755 \
    /usr/local/bin/codex-a2a \
    /usr/local/bin/codex \
    /usr/local/bin/docker-entrypoint.sh

EXPOSE 9001

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["codex-a2a"]
