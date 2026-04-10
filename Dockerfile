# ---------- stage 1: build ----------
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/log-sidecar ./main.go

# ---------- stage 2: runtime ----------
FROM alpine:3.20

COPY --from=builder /bin/log-sidecar /usr/local/bin/log-sidecar

ENTRYPOINT ["/usr/local/bin/log-sidecar"]