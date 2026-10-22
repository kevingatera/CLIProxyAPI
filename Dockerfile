FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates curl && rm -rf /var/lib/apt/lists/*

# Cursor Agent CLI for cursor-based auth flows in the management API.
# Cursor publishes no checksums; these hashes were computed from the pinned
# packages. To bump, update the version and both hashes together.
# Set INSTALL_CURSOR_AGENT=false to build without it.
ARG INSTALL_CURSOR_AGENT=true
ARG CURSOR_AGENT_VERSION=2026.10.19-6bbf2c4
ARG CURSOR_AGENT_SHA256_AMD64=cf80cc18d8b33bf33e762353977ca1868cfc45290c92c0ffdcef83bb8b37b44f
ARG CURSOR_AGENT_SHA256_ARM64=71a90f12a8b8183a71442b7ed4037bddc1a0f88114ec4fb9ec92782acb8e7d84
ARG TARGETARCH

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

RUN if [ "${INSTALL_CURSOR_AGENT}" != "true" ]; then exit 0; fi; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) arch=x64; sum="${CURSOR_AGENT_SHA256_AMD64}" ;; \
      arm64) arch=arm64; sum="${CURSOR_AGENT_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    dir="/root/.local/share/cursor-agent/versions/${CURSOR_AGENT_VERSION}"; \
    curl -fsSL -o /tmp/cursor-agent.tar.gz \
      "https://downloads.cursor.com/lab/${CURSOR_AGENT_VERSION}/linux/${arch}/agent-cli-package.tar.gz"; \
    echo "${sum}  /tmp/cursor-agent.tar.gz" | sha256sum -c -; \
    mkdir -p "${dir}" /root/.local/bin; \
    tar --strip-components=1 -xzf /tmp/cursor-agent.tar.gz -C "${dir}"; \
    rm /tmp/cursor-agent.tar.gz; \
    test -x "${dir}/cursor-agent"; \
    for name in cursor-agent agent; do \
      ln -s "${dir}/cursor-agent" "/root/.local/bin/${name}"; \
      ln -s "${dir}/cursor-agent" "/usr/local/bin/${name}"; \
    done; \
    test "$(cursor-agent --version)" = "${CURSOR_AGENT_VERSION}"

SHELL ["/bin/sh", "-c"]

ENV PATH="/root/.local/bin:${PATH}"

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
