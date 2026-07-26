# Build stage
# --platform=$BUILDPLATFORM keeps the toolchain running natively on the build
# host; the Go cross-compiler produces the target binary, so no emulation is
# needed when building for a foreign architecture.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary for the requested target platform. TARGETOS/TARGETARCH are
# provided automatically by BuildKit; the defaults keep the previous behaviour
# for builders that don't set them.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o twitchpoint ./cmd/twitchpoint

# Runtime stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/twitchpoint .

# Config and logs volumes
VOLUME /app/config
VOLUME /app/logs

# Web UI port
EXPOSE 8080

# Run in headless mode (no TUI) with config from volume
ENTRYPOINT ["./twitchpoint", "--headless", "--config", "/app/config/config.json"]
