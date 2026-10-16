#!/usr/bin/env sh
# Runs the oh-my-mermaid CLI (`omm`) inside its container against this checkout, so the
# .omm/ architecture docs can be written and validated without installing Node on the
# host. Usage: scripts/omm.sh <omm args>   e.g. scripts/omm.sh write overall-architecture description -
set -eu
cd "$(dirname "$0")/.."
HOST_UID=$(id -u) HOST_GID=$(id -g) docker compose --profile tools run --rm --no-deps -T omm "$@"
