# Multi-stage Dockerfile for resy-snipe v2 per docs/v2/design/daemon.md §9.3.
#
# Stage 1 ("builder"): a pinned golang image compiles a static-ish
# binary. CGO is disabled because modernc.org/sqlite is pure-Go, so the
# resulting binary needs no libc and runs cleanly in the distroless
# runtime stage.
#
# Stage 2 ("runtime"): gcr.io/distroless/base-debian12. Non-root by
# convention (uid 65532, the nonroot user distroless ships). The signer
# binary, if any, is mounted at runtime (RESY_SNIPE_SIGNER_BIN) rather
# than baked in — see deploy/docker/docker-compose.yml.example.

ARG GO_VERSION=1.25

# -----------------------------------------------------------------------------
# Stage 1: builder
# -----------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

# git is needed by `go mod download` for VCS-stamped builds; no other
# tools required because the dependency tree is pure Go.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache the module graph independently of source changes so iterative
# builds only re-fetch when go.{mod,sum} actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build flags:
#   CGO_ENABLED=0      pure-Go binary; no libc dependency in the runtime image.
#   -trimpath          strip filesystem paths from the binary for reproducibility.
#   -ldflags "-s -w"   drop symbol/debug tables; halves the binary size.
#   -ldflags X=...     stamp serveBuildVersion / serveBuildCommit so /healthz
#                      and the boot banner report the build under test.
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 \
    go build \
      -trimpath \
      -ldflags "-s -w \
        -X 'main.serveBuildVersion=${VERSION}' \
        -X 'main.serveBuildCommit=${COMMIT}'" \
      -o /out/resy-snipe \
      ./cmd/resy-snipe

# -----------------------------------------------------------------------------
# Stage 2: runtime (distroless)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/base-debian12:nonroot

# Distroless ships a nonroot user at uid 65532; we run as that. The
# state directory is created with the right ownership at image-build
# time so a mounted volume inherits an owner that the running user can
# write to (Docker bind-mounts respect the source uid by default, so
# operators should `chown -R 65532:65532 ./data` on the host).
USER nonroot:nonroot

# A daemon-managed data directory: SQLite db, lock file, any future
# sealed-blob spool. The systemd unit uses /var/lib/resy-snipe; the
# container does the same so config.toml is portable across both
# deployment shapes.
WORKDIR /var/lib/resy-snipe

# /etc/resy-snipe is read-only at runtime (the operator mounts
# config.toml and keyfile here via -v or Docker secrets). We don't
# create it inside the image because distroless doesn't ship coreutils;
# a bind-mount that references a missing path will fail loudly anyway,
# which is the diagnostic we want.

# Copy the static binary. /usr/local/bin matches the systemd unit's
# ExecStart path so an operator switching between shapes doesn't have
# to relearn anything.
COPY --from=builder /out/resy-snipe /usr/local/bin/resy-snipe

# Expose the loopback listener port. The container binds 0.0.0.0
# inside its own network namespace per design §9.3; Docker's
# `127.0.0.1:7765:7765` mapping at the host side is what constrains
# exposure. A reverse proxy (Caddy/Traefik/nginx) handles TLS.
EXPOSE 7765

# Default command: serve. Operators can override (e.g. `docker run
# resy-snipe migrate-secrets --dry-run`) without rebuilding the image.
ENTRYPOINT ["/usr/local/bin/resy-snipe"]
CMD ["serve", "--config", "/etc/resy-snipe/config.toml"]
