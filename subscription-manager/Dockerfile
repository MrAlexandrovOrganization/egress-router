FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod main.go main_test.go ./
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -o /subscription-manager .

FROM ghcr.io/sagernet/sing-box:v1.14.0@sha256:4bed9332a0013fef72c31200a84e8fc0ed91a5ab2fe373a69f0acbbbbfbef3c5 AS singbox
FROM alpine:3.21
COPY --from=builder /subscription-manager /app/subscription-manager
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
ENTRYPOINT ["/app/subscription-manager"]
