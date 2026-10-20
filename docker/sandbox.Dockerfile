# syntax=docker/dockerfile:1.7
# The image OpenClaw uses for agent sandboxes (agents.defaults.sandbox.docker.image).
# Agent shell commands run here: no network, read-only root, workspace bind-mounted by
# the gateway. Keep it small and deterministic; pin what the demo needs.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates libgomp1 \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir pytest==8.3.3 \
    && useradd -u 1000 -m -s /bin/bash sandbox
USER sandbox
WORKDIR /workspace
# OpenClaw keeps the container alive and execs into it; a sleeping entrypoint is the
# documented shape for sandbox images.
CMD ["sleep", "infinity"]
