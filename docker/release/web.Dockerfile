# Release-only packaging for the exact source allowlist. Every base and direct
# download is immutable; the workflow separately verifies these values against
# release/exact-sources.json before executing this file.
FROM --platform=linux/amd64 docker.io/library/node:22-alpine@sha256:76789712cd1ae89a1225eac9077010d68987a423588042dac30446f502f1858c AS frontend-builder

WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ .
RUN npm run build

FROM --platform=linux/amd64 docker.io/library/golang:1.26.6-alpine@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV GOPROXY=https://proxy.golang.org
ENV GOSUMDB=sum.golang.org
ENV GOTOOLCHAIN=local
WORKDIR /app
RUN test "${TARGETOS}" = linux && test "${TARGETARCH}" = amd64
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend-builder /app/frontend/dist/ ./internal/frontend/dist/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -a -installsuffix cgo -o rereply ./cmd/whatomate

FROM --platform=linux/amd64 docker.io/library/node:22-alpine@sha256:76789712cd1ae89a1225eac9077010d68987a423588042dac30446f502f1858c AS piper-download

ADD --checksum=sha256:a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992 https://github.com/rhasspy/piper/releases/download/2023.11.14-2/piper_linux_x86_64.tar.gz /tmp/piper.tar.gz
RUN tar xf /tmp/piper.tar.gz -C /tmp && rm /tmp/piper.tar.gz
ADD --checksum=sha256:5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f https://huggingface.co/rhasspy/piper-voices/resolve/39ab474be869e9181350af6a65e4953eef67aaa0/en/en_US/lessac/medium/en_US-lessac-medium.onnx /tmp/piper-models/en_US-lessac-medium.onnx
ADD --checksum=sha256:efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0 https://huggingface.co/rhasspy/piper-voices/resolve/39ab474be869e9181350af6a65e4953eef67aaa0/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json /tmp/piper-models/en_US-lessac-medium.onnx.json
RUN chmod 0644 \
      /tmp/piper-models/en_US-lessac-medium.onnx \
      /tmp/piper-models/en_US-lessac-medium.onnx.json

FROM --platform=linux/amd64 docker.io/library/ubuntu:24.04@sha256:1e0a86e57d247923571b75e0aaf48a1449cf8c543d51fb3e07a4a7d7bfa79316

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ARG UBUNTU_SNAPSHOT=20260824T000000Z
WORKDIR /app
RUN set -eu; \
    printf 'APT::Snapshot "%s";\n' "$UBUNTU_SNAPSHOT" > /etc/apt/apt.conf.d/50snapshot \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive TZ=Etc/UTC apt-get install -y --no-install-recommends \
      ca-certificates=20260601~24.04.1 \
      tzdata=2026c-0ubuntu0.24.04.1 \
      espeak-ng=1.51+dfsg-12build1 \
      opus-tools=0.2-1build3 \
      ffmpeg=7:6.1.1-3ubuntu5 \
    && for package in ca-certificates tzdata espeak-ng opus-tools ffmpeg; do \
         apt-cache policy "$package" | grep -F 'snapshot.ubuntu.com' > /dev/null || exit 1; \
       done \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/rereply ./rereply
COPY --from=builder /app/config.example.toml ./config.toml
COPY --from=piper-download /tmp/piper/piper /usr/local/bin/piper
COPY --from=piper-download /tmp/piper/lib*.so* /usr/local/lib/
COPY --from=piper-download /tmp/piper/espeak-ng-data /usr/share/espeak-ng-data
RUN ldconfig
COPY --from=piper-download /tmp/piper-models /opt/piper/models
RUN groupadd --system rereply \
    && useradd --system --gid rereply --home /app --shell /usr/sbin/nologin rereply \
    && mkdir -p /app/uploads /app/audio \
    && chown -R rereply:rereply /app/uploads /app/audio

EXPOSE 8080
USER rereply
ENTRYPOINT ["./rereply"]
CMD ["server", "-config", "config.toml"]
