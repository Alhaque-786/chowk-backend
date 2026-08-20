# Multi-stage build. The final image contains a static binary and nothing else.
#
# Why it matters: the builder stage is ~900MB (Go toolchain, module cache, source).
# The runtime stage is ~15MB. That difference is pull time on every deploy and
# every autoscale event, and — more importantly — attack surface. A scratch image
# has no shell, no package manager, and no libc, so a container escape has nothing
# to pivot with.

# ---------------------------------------------------------------------------
# Stage 1: build
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# git is needed for module resolution; ca-certificates is copied into the final
# image so the binary can make outbound TLS calls (payment gateway, SMS).
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Copy manifests first, alone. This layer is cached and only invalidated when
# dependencies actually change, so ordinary code edits skip the whole download.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# CGO_ONABLED=0 produces a statically linked binary, which is what allows a
# scratch base image at all — with cgo, the binary would need libc at runtime.
#
# -trimpath strips local filesystem paths from the binary. Without it, panics in
# production leak your build machine's directory layout.
# -s -w drop the symbol table and DWARF debug info, roughly halving binary size.
# Keep them off if you need readable production stack traces.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/migrate ./cmd/migrate

# ---------------------------------------------------------------------------
# Stage 2: runtime
# ---------------------------------------------------------------------------
FROM scratch AS runtime

# TLS root certificates. Without these every outbound HTTPS call fails with
# "x509: certificate signed by unknown authority" — a confusing first-deploy
# failure that looks like a network problem.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Timezone data, so Asia/Kolkata resolves for kitchen opening hours. Without it
# time.LoadLocation fails and the code falls back to a fixed offset.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /out/api /api
COPY --from=builder /out/worker /worker
COPY --from=builder /out/migrate /migrate
COPY --chown=65532:65532 migrations /migrations

# Run unprivileged. 65532 is the conventional "nonroot" uid; a numeric uid is
# used rather than a name because scratch has no /etc/passwd to resolve one.
USER 65532:65532

EXPOSE 8080

# No HEALTHCHECK directive: scratch has no shell or curl to run one, and the
# orchestrator should own probing anyway via /health and /ready. A Docker-level
# healthcheck would duplicate that and disagree with it.

ENTRYPOINT ["/api"]
