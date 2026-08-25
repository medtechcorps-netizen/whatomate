FROM --platform=linux/amd64 docker.io/library/golang:1.26.6-alpine@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV GOPROXY=https://proxy.golang.org
ENV GOSUMDB=sum.golang.org
ENV GOTOOLCHAIN=local
WORKDIR /src
RUN test "${TARGETOS}" = linux && test "${TARGETARCH}" = amd64
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags timetzdata \
      -trimpath -ldflags="-s -w" -o /out/meta-relay ./cmd/meta-relay \
    && printf 'relay:x:65532:65532:ReReply relay:/app:/sbin/nologin\n' > /out/passwd \
    && printf 'relay:x:65532:\n' > /out/group

FROM scratch

WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/passwd /etc/passwd
COPY --from=builder /out/group /etc/group
COPY --from=builder /out/meta-relay /app/meta-relay

USER relay
EXPOSE 8081
ENTRYPOINT ["/app/meta-relay"]
