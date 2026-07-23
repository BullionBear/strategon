# Control plane image: SPA build -> Go build with the SPA embedded -> scratch.
# The result is one statically linked binary serving gRPC (agents), the human
# JSON API, and the UI.

# --- stage 1: frontend ---
# Expects web/static/openapi.json from `make generate` (committed in-repo).
# The SPA /api page loads that file at runtime as /openapi.json.
FROM node:22-alpine AS fe
WORKDIR /web
ENV CI=true
RUN corepack enable
# Lockfile first so dependency installs stay cached across source-only changes.
# pnpm-workspace.yaml carries allowBuilds (esbuild) — required by pnpm v11 or
# install exits with ERR_PNPM_IGNORED_BUILDS.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN test -f static/openapi.json \
  || (echo "missing static/openapi.json — run 'make generate' before docker build" && exit 1)
RUN pnpm build

# --- stage 2: go build (embeds the SPA) ---
FROM golang:1.25-alpine AS builder
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

# --- stage 3: runtime filesystem bits (scratch has no /tmp) ---
FROM alpine:3.22 AS runtime-fs
RUN mkdir -p /tmp && chmod 1777 /tmp

# --- stage 4: runtime ---
FROM scratch
# Links the GHCR package to this repository, which is what lets a workflow's
# GITHUB_TOKEN push to it. Without the link, pushes are 403 regardless of the
# workflow's `packages: write` permission.
LABEL org.opencontainers.image.source="https://github.com/BullionBear/strategon"
LABEL org.opencontainers.image.description="Strategon control plane (UI + human API + AgentService)"
# Needed for the outbound TLS call to Discord's OAuth endpoints.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Postgres DSN parsing and OAuth state both want a real clock/zone table.
COPY --from=builder /usr/local/go/lib/time/zoneinfo.zip /usr/local/go/lib/time/zoneinfo.zip
ENV ZONEINFO=/usr/local/go/lib/time/zoneinfo.zip
# Ingest (and any other os.CreateTemp) need a writable /tmp in scratch.
COPY --from=runtime-fs /tmp /tmp
COPY --from=builder /out/controlplane /controlplane
# 8080 agents (mTLS), 8081 humans (behind Traefik).
EXPOSE 8080 8081
ENTRYPOINT ["/controlplane"]
