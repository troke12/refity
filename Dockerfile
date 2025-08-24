# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS builder
WORKDIR /app
# Install build dependencies for SQLite
RUN apk add --no-cache gcc musl-dev sqlite-dev
COPY . .
RUN CGO_ENABLED=1 go mod tidy && go build -o refity ./cmd/refity

FROM alpine:latest
WORKDIR /app
# Install runtime dependencies for SQLite
RUN apk add --no-cache sqlite
COPY --from=builder /app/refity .
EXPOSE 5000
CMD ["./refity"] 