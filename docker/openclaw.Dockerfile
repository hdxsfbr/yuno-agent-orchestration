# syntax=docker/dockerfile:1.7
# The OpenClaw gateway, pinned, plus the static docker CLI. The gateway creates agent
# sandboxes by running `docker inspect` / `docker exec` / `docker run` against the host
# socket (DESIGN 5.14 rule 12); the upstream image ships no CLI.
ARG OPENCLAW_VERSION=2026.10.3
FROM ghcr.io/openclaw/openclaw:${OPENCLAW_VERSION}

ARG DOCKER_CLI_VERSION=28.3.3
USER root
RUN set -eux; \
    curl -fsSL "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_CLI_VERSION}.tgz" \
      | tar -xz -C /usr/local/bin --strip-components=1 docker/docker; \
    chmod 0755 /usr/local/bin/docker; \
    /usr/local/bin/docker --version
USER node
