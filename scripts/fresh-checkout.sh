#!/usr/bin/env sh
# The reader's path: clone the repository into an empty directory, copy .env.example to
# .env (no secrets), make run, make smoke, make test-go, then take it all down.
# Run it with the development stack stopped (both use port 8080 and the postgres port).
# Usage: scripts/fresh-checkout.sh [git-url-or-path] [workdir]
set -eu
SRC=${1:-$(cd "$(dirname "$0")/.." && pwd)}
WORK=${2:-$(mktemp -d /tmp/yuno-fresh.XXXXXX)}
echo "cloning $SRC into $WORK"
git clone -q "$SRC" "$WORK/yuno"
cd "$WORK/yuno"
cp .env.example .env
start=$(date +%s)
make run
echo "make run took $(( $(date +%s) - start )) s"
make smoke
make test-go
make clean
echo "fresh checkout: run, smoke and the Go suite passed from an empty clone in $WORK/yuno"
