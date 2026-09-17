# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Copy dependency manifests first so Docker can cache this layer when only
# source files change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary — required for distroless.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /gateway ./cmd

# ---- final stage ----
# gcr.io/distroless/static-debian12 has no shell, no package manager, and no
# libc — the attack surface is as small as it gets for a static Go binary.
FROM gcr.io/distroless/static-debian12

# Run as a non-root UID. distroless exposes uid 65532 (nonroot) by default.
USER nonroot:nonroot

COPY --from=builder /gateway /gateway

EXPOSE 8080

ENTRYPOINT ["/gateway"]
