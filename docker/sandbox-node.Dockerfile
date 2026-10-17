# syntax=docker/dockerfile:1.7
# Sandbox image for project repositories whose toolchain is node (DESIGN 5.16 rule 4):
# node 24 with npm, pnpm and the yarn the upstream image ships, plus git. Nothing project-specific is baked in; a run
# with sandbox network installs the project's own dependencies from its lockfile. The
# root filesystem is read-only at runtime, so every cache lives in the agent's
# workspace (bind-mounted read-write at /workspace), in hidden directories the platform
# never copies forward or packages. The gateway sets HOME=/workspace and its own PATH
# for every exec [probed], so tools must be in /usr/local/bin and HOME is the workspace. The image's node user is uid 1000, the sandbox user.
FROM node:24-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g --no-fund --no-audit pnpm@10.34.7 \
    && npm cache clean --force
ENV XDG_CACHE_HOME=/workspace/.cache \
    npm_config_cache=/workspace/.cache/npm \
    npm_config_update_notifier=false \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    YARN_CACHE_FOLDER=/workspace/.cache/yarn
USER node
WORKDIR /workspace
CMD ["sleep", "infinity"]
