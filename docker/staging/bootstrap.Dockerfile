FROM --platform=linux/amd64 docker.io/library/golang:1.26.6-alpine@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p internal/frontend/dist && touch internal/frontend/dist/.gitkeep
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o /bootstrap ./release/staging/bootstrap
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /bootstrap /bootstrap
USER 65532:65532
ENTRYPOINT ["/bootstrap"]
