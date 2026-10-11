# Context is restricted to this recipe and the three COPY inputs below.
# The caller binds both build arguments to the clean, reviewed source commit.
FROM --platform=linux/amd64 docker.io/library/python:3.13-alpine@sha256:ba27df8b6499cc3756cff167690f3fcb5321faa1703b3a914df5f4fdadb32f05

ARG SOURCE_SHA
ARG RECIPE_SHA256
LABEL org.opencontainers.image.source="https://github.com/medtechcorps-netizen/whatomate" \
      org.opencontainers.image.revision="${SOURCE_SHA}" \
      codex.compat.role="controller" \
      codex.compat.recipe-sha256="${RECIPE_SHA256}"

WORKDIR /compat
COPY --chown=65532:65532 release/compat/controller.py release/compat/sweep.py release/compat/proof.py ./
USER 65532:65532
ENTRYPOINT ["python3", "-I", "-B", "/compat/controller.py"]
CMD []
