# Staging and local only: the synthetic Meta Graph API (release/staging/graphstub).
# No release workflow builds this file, and no release binary contains the stub
# (release/staging/graphstub/placement_test.go). The stub uses the standard
# library only, so the build needs go.mod, go.sum and its own sources, and
# graph-stub.Dockerfile.dockerignore limits the BuildKit context to exactly those.
FROM --platform=linux/amd64 docker.io/library/golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV GOTOOLCHAIN=local
ENV GOFLAGS=-mod=readonly
WORKDIR /src
RUN test "${TARGETOS}" = linux && test "${TARGETARCH}" = amd64
COPY go.mod go.sum ./
COPY release/staging/graphstub/ ./release/staging/graphstub/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
      -o /out/graph-stub ./release/staging/graphstub/cmd/graph-stub \
    && printf 'stub:x:65532:65532:Graph stub:/:/sbin/nologin\n' > /out/passwd \
    && printf 'stub:x:65532:\n' > /out/group

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/passwd /etc/passwd
COPY --from=builder /out/group /etc/group
COPY --from=builder /out/graph-stub /graph-stub

USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/graph-stub"]
