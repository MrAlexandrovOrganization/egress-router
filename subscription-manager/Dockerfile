# check=skip=InvalidDefaultArgInFrom

# Required version arguments are supplied by Make/Compose, without fallback pins.
ARG GO_VERSION
ARG ALPINE_VERSION
ARG SING_BOX_IMAGE
FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
COPY go.mod main.go main_test.go ./
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -o /subscription-manager .

FROM ${SING_BOX_IMAGE} AS singbox
FROM alpine:${ALPINE_VERSION}
COPY --from=builder /subscription-manager /app/subscription-manager
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
ENTRYPOINT ["/app/subscription-manager"]
