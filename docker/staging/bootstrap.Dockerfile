FROM --platform=linux/amd64 docker.io/library/golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS builder
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
COPY internal/dbcatalog/golden/production-v0.json /production-shape/production-v0.json
COPY internal/dbcatalog/shape/production-v0.sql /production-shape/production-v0.sql
USER 65532:65532
ENTRYPOINT ["/bootstrap"]
