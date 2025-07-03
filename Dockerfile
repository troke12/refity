# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o refity ./cmd/refity

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/refity .
EXPOSE 5000
CMD ["./refity"] 