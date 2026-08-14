ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.25.13-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/gmail-relay ./cmd/gmail-relay

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S relay \
    && adduser -S -G relay -h /app relay

WORKDIR /app
COPY --from=builder /out/gmail-relay /app/gmail-relay

USER relay
EXPOSE 8082
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8082/readyz || exit 1

ENTRYPOINT ["/app/gmail-relay"]
