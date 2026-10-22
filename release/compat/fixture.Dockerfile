# Build from an exact Git source snapshot. The test-only exporter accepts its
# private fixture input on stdin and requires literal loopback PostgreSQL URLs.
FROM --platform=linux/amd64 docker.io/library/golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -mod=readonly -trimpath -c -o /database.test ./internal/database

FROM scratch
ARG SOURCE_SHA
ARG RECIPE_SHA256
LABEL org.opencontainers.image.source="https://github.com/medtechcorps-netizen/whatomate" \
      org.opencontainers.image.revision="${SOURCE_SHA}" \
      codex.compat.role="fixture" \
      codex.compat.recipe-sha256="${RECIPE_SHA256}"
COPY --from=builder /database.test /compat/database.test
COPY internal/dbcatalog/golden/production-v0.json /src/internal/dbcatalog/golden/production-v0.json
COPY internal/dbcatalog/shape/production-v0.sql /src/internal/dbcatalog/shape/production-v0.sql
WORKDIR /src/internal/database
USER 65532:65532
ENTRYPOINT ["/compat/database.test"]
CMD []
