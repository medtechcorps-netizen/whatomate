FROM --platform=linux/amd64 mcr.microsoft.com/playwright:v1.57.0-noble@sha256:8fb7af3bb488c51364d6554876a8eddf377736608327dbdf4177b4901faf7bc9

ENV NODE_ENV=production \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

WORKDIR /app

COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --include=dev --ignore-scripts \
    && npm cache clean --force

COPY frontend/canary-driver/ ./canary-driver/

RUN chown -R pwuser:pwuser /app
USER pwuser

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:8080/healthz').then(r=>{if(r.status!==204)process.exit(1)}).catch(()=>process.exit(1))"]

ENTRYPOINT ["node", "canary-driver/index.mjs"]
