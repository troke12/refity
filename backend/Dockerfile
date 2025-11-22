# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.23-alpine AS builder

# Install build dependencies for SQLite
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o server ./cmd/server

# Production stage
FROM alpine:latest

# Install runtime dependencies for SQLite
RUN apk --no-cache add ca-certificates sqlite

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/server .

# Create data directory
RUN mkdir -p /app/data

# Expose port
EXPOSE 5000

# Run the server
CMD ["./server"]

