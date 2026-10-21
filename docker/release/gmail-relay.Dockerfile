FROM --platform=linux/amd64 docker.io/library/golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder

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
      -trimpath -ldflags="-s -w" -o /out/gmail-relay ./cmd/gmail-relay \
    && printf 'relay:x:65532:65532:ReReply relay:/app:/sbin/nologin\n' > /out/passwd \
    && printf 'relay:x:65532:\n' > /out/group

FROM scratch

WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/passwd /etc/passwd
COPY --from=builder /out/group /etc/group
COPY --from=builder /out/gmail-relay /app/gmail-relay

USER relay
EXPOSE 8082
ENTRYPOINT ["/app/gmail-relay"]
