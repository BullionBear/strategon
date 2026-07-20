# Control plane image: SPA build -> Go build with the SPA embedded -> scratch.
# The result is one statically linked binary serving gRPC (agents), the human
# JSON API, and the UI (CICD.md §3.3).

# --- stage 1: frontend ---
FROM node:22-alpine AS fe
WORKDIR /web
RUN corepack enable
# Lockfile first so dependency installs stay cached across source-only changes.
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# --- stage 2: go build (embeds the SPA) ---
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Must land before `go build`: //go:embed reads the tree at compile time.
COPY --from=fe /web/build ./internal/webassets/dist

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
# Injected by buildx, one build per --platform entry.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X github.com/bullionbear/strategon/internal/buildinfo.Version=${VERSION} \
      -X github.com/bullionbear/strategon/internal/buildinfo.CommitHash=${COMMIT} \
      -X github.com/bullionbear/strategon/internal/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/controlplane ./cmd/controlplane

# --- stage 3: runtime ---
FROM scratch
# Needed for the outbound TLS call to Discord's OAuth endpoints.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Postgres DSN parsing and OAuth state both want a real clock/zone table.
COPY --from=builder /usr/local/go/lib/time/zoneinfo.zip /usr/local/go/lib/time/zoneinfo.zip
ENV ZONEINFO=/usr/local/go/lib/time/zoneinfo.zip
COPY --from=builder /out/controlplane /controlplane
# 8080 agents (mTLS), 8081 humans (behind Traefik).
EXPOSE 8080 8081
ENTRYPOINT ["/controlplane"]
