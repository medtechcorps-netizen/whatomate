FROM --platform=linux/amd64 mcr.microsoft.com/playwright:v1.57.0-noble@sha256:8fb7af3bb488c51364d6554876a8eddf377736608327dbdf4177b4901faf7bc9

ENV NODE_ENV=production \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends --only-upgrade \
      dirmngr \
      gpg \
      gpg-agent \
      gpgconf \
      gpgv \
      libssl3t64 \
      openssl; \
    for package in dirmngr gpg gpg-agent gpgconf gpgv; do \
      version="$(dpkg-query -W -f='${Version}' "$package")"; \
      dpkg --compare-versions "$version" ge '2.4.4-2ubuntu17.4'; \
    done; \
    for package in libssl3t64 openssl; do \
      version="$(dpkg-query -W -f='${Version}' "$package")"; \
      dpkg --compare-versions "$version" ge '3.0.13-0ubuntu3.11'; \
    done; \
    apt-get purge -y \
      gstreamer1.0-plugins-bad \
      libgstreamer-plugins-bad1.0-0; \
    for package in gstreamer1.0-plugins-bad libgstreamer-plugins-bad1.0-0; do \
      status="$(dpkg-query -W -f='${db:Status-Abbrev}' "$package" 2>/dev/null || true)"; \
      [ "$status" != 'ii ' ]; \
    done; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --include=dev --ignore-scripts \
    && npm cache clean --force \
    && rm -rf \
      /root/.npm \
      /usr/lib/node_modules/npm \
      /usr/lib/node_modules/yarn \
      /usr/local/lib/node_modules/npm \
      /usr/local/lib/node_modules/yarn \
    && rm -f \
      /usr/bin/npm \
      /usr/bin/npx \
      /usr/bin/yarn \
      /usr/bin/yarnpkg \
      /usr/local/bin/npm \
      /usr/local/bin/npx \
      /usr/local/bin/yarn \
      /usr/local/bin/yarnpkg

COPY frontend/canary-driver/ ./canary-driver/

RUN chown -R pwuser:pwuser /app
USER pwuser

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:8080/healthz').then(r=>{if(r.status!==204)process.exit(1)}).catch(()=>process.exit(1))"]

ENTRYPOINT ["node", "canary-driver/index.mjs"]
