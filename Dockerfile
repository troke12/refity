# syntax=docker/dockerfile:1

# Frontend build stage
FROM node:22-alpine AS frontend-builder
WORKDIR /app
COPY package*.json ./
RUN npm install
COPY . .
RUN npm run build

# Go build stage
FROM golang:1.23-alpine AS builder
WORKDIR /app
# Install build dependencies for SQLite
RUN apk add --no-cache gcc musl-dev sqlite-dev
COPY . .
# Copy built CSS from frontend stage
COPY --from=frontend-builder /app/static ./static
RUN CGO_ENABLED=1 go mod tidy && go build -o refity ./cmd/refity

# Final stage
FROM alpine:latest
WORKDIR /app
# Install runtime dependencies for SQLite
RUN apk add --no-cache sqlite
COPY --from=builder /app/refity .
COPY --from=frontend-builder /app/static ./static
EXPOSE 5000
CMD ["./refity"] 