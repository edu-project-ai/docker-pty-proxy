# ---------- Build stage ----------
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/docker-pty-proxy ./cmd/server

# ---------- Runtime stage ----------
FROM alpine:3.19

RUN apk add --no-cache ca-certificates
COPY --from=builder /out/docker-pty-proxy /usr/local/bin/docker-pty-proxy

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docker-pty-proxy"]
