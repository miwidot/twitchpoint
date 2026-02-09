# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o twitchpoint ./cmd/twitchpoint

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/twitchpoint .

# Config volume
VOLUME /app/config

# Web UI port
EXPOSE 8080

# Run with config from volume
ENTRYPOINT ["./twitchpoint", "--config", "/app/config/config.json"]
