FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends tzdata bash curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Install Cursor Agent CLI for cursor-based auth flows in management API.
RUN curl -fsSL https://cursor.com/install | bash \
 && ln -sf /root/.local/bin/cursor-agent /usr/local/bin/cursor-agent \
 && ln -sf /root/.local/bin/agent /usr/local/bin/agent

ENV PATH="/root/.local/bin:${PATH}"

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN ln -snf /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
