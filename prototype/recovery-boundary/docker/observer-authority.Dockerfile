FROM --platform=linux/amd64 docker.io/library/golang:1.26.6-alpine@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae AS builder
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local GOPROXY=off
WORKDIR /src
COPY go.mod ./
COPY prototype/recovery-boundary ./prototype/recovery-boundary
RUN go build -trimpath -ldflags="-s -w" -o /out/runtime ./prototype/recovery-boundary/cmd/observer-authority

FROM scratch
COPY --from=builder /out/runtime /runtime
USER 65532:65532
ENTRYPOINT ["/runtime"]
