# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git build-base

# Download dependencies first (better layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o app -ldflags="-w -s" .

# Final stage
FROM alpine:3

# Create a non-root user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Install runtime dependencies
RUN apk update && \
  apk upgrade && \
  apk add --no-cache \
  ca-certificates \
  tzdata && \
  rm -rf /var/cache/apk/*

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/app .

# Switch to non-root user
USER appuser

# Run the application
CMD ["./app"]
