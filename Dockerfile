# syntax=docker/dockerfile:1.7

FROM golang:1.26.8-alpine3.23 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/x-ui-tgbot \
    ./cmd/bot

FROM alpine:3.23 AS runtime

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app \
    && adduser -S -G app -H -s /sbin/nologin app

COPY --from=builder --chown=app:app /out/x-ui-tgbot /usr/local/bin/x-ui-tgbot

USER app:app
WORKDIR /app

ENTRYPOINT ["/usr/local/bin/x-ui-tgbot"]

