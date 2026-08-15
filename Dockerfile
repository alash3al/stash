# syntax=docker/dockerfile:1

# ------------------------------------------
# Stage 1: Builder
# ------------------------------------------
# Use --platform=$BUILDPLATFORM so this stage runs natively on your Mac (fast),
# regardless of what architecture the final image is targeting.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Install ca-certificates in builder so we can copy them to the final image.
RUN apk --no-cache add ca-certificates

WORKDIR /src

# 1. Dependency caching (Docker only re-downloads if these files change)
COPY go.mod go.sum ./
RUN go mod download

# 2. Copy source code
COPY . .

# 3. Define build arguments for the TARGET platform.
# These are automatically populated by Docker Buildx.
ARG TARGETOS
ARG TARGETARCH

# 4. Build the binary.
# CGO_ENABLED=0 creates a fully static binary (no C library dependencies).
# -ldflags="-s -w" strips debug info for a smaller binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /out/stash \
    ./cmd/cli/

# ------------------------------------------
# Stage 2: Final (Production)
# ------------------------------------------
# Distroless images are the industry standard for Go microservices.
# They contain ONLY the binary and runtime dependencies (like certs).
# No shell, no package manager, no user tools -> Extremely secure.
FROM gcr.io/distroless/static:nonroot

# Copy CA certificates from builder so the binary can make HTTPS requests.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the static binary.
COPY --from=builder /out/stash /stash

# Security: Run as non-root user (provided by distroless image).
USER nonroot:nonroot

# Default command - show help
ENTRYPOINT ["/stash"]
CMD ["serve"]
