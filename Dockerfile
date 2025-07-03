# syntax=docker/dockerfile:1
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o refity ./cmd/refity

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/refity .
COPY .env.example .env
EXPOSE 5000
CMD ["./refity"] 