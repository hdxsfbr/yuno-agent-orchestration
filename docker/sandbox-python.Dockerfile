# syntax=docker/dockerfile:1.7
# Sandbox image for project repositories whose toolchain is python (DESIGN 5.16 rule 4):
# python 3.12, pip, uv and pytest, plus git. Nothing project-specific is baked in; a run
# with sandbox network installs the project's own dependencies into .venv. The root
# filesystem is read-only at runtime, so every cache lives in the agent's
# workspace (bind-mounted read-write at /workspace), in hidden directories the platform
# never copies forward or packages. The gateway sets HOME=/workspace and its own PATH
# for every exec [probed], so tools must be in /usr/local/bin and HOME is the workspace.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl libgomp1 \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir pytest==9.1.1 uv==0.13.1 \
    && useradd -u 1000 -m -s /bin/bash sandbox
ENV XDG_CACHE_HOME=/workspace/.cache \
    PIP_CACHE_DIR=/workspace/.cache/pip \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    UV_CACHE_DIR=/workspace/.cache/uv \
    UV_PYTHON_INSTALL_DIR=/workspace/.cache/uv-python
USER sandbox
WORKDIR /workspace
# OpenClaw keeps the container alive and execs into it; a sleeping entrypoint is the
# documented shape for sandbox images.
CMD ["sleep", "infinity"]
