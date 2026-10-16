#!/bin/sh
# Container entrypoint for the `demo` service: playwright-core is not part of the official image (only
# its browsers are), so it is installed once into PW_DIR, a git-ignored directory under scripts/demo
# that persists on the host between runs. The version must match the browsers in the image tag.
set -eu
PW_VERSION="${PW_VERSION:-1.49.1}"
PW_DIR="${PW_DIR:-/demo/.pw}"
mkdir -p "$PW_DIR"
have="$(node -e "try{console.log(require('$PW_DIR/node_modules/playwright-core/package.json').version)}catch(e){}" 2>/dev/null || true)"
if [ "$have" != "$PW_VERSION" ]; then
  echo "installing playwright-core@$PW_VERSION into $PW_DIR"
  [ -f "$PW_DIR/package.json" ] || echo '{"name":"demo-playwright","private":true}' > "$PW_DIR/package.json"
  (cd "$PW_DIR" && npm install --no-audit --no-fund --loglevel=error "playwright-core@$PW_VERSION" < /dev/null)
fi
exec node /demo/record.mjs "$@" < /dev/null
