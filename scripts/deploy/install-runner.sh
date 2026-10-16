#!/usr/bin/env bash
# Installs a GitLab Runner (shell executor) on this host and registers it for the project, so
# the pipeline's jobs can run here. Run as root on the host over ssh. The runner
# authentication token travels as the FIRST LINE of stdin, ahead of the script, so it is
# never on a command line or in a shell history:
#   { cat runner-token.txt; cat scripts/deploy/install-runner.sh; } \
#     | ssh root@HOST 'read -r RUNNER_TOKEN; export RUNNER_TOKEN; bash -s -- --tags yuno-prod [--user yuno] [--url https://labs.gauntletai.com]'
# The service runs as --user so deploy jobs can reach that user's checkout, .env and the
# docker group. Create the runner in GitLab (Settings > CI/CD > Runners, or POST /user/runners)
# with the same tag and "run untagged" off; --tags here only labels the description.
# Idempotent for the install; registration is skipped when this token is already registered.
set -euo pipefail

# This script arrives on bash's stdin over ssh: one function, read fully before anything
# runs, called with stdin from /dev/null so no child command can swallow the rest of it.
main() {
  TAGS="" USER_NAME=yuno URL=https://labs.gauntletai.com
  while [ $# -gt 0 ]; do
    case "$1" in
      --tags) TAGS="$2"; shift 2 ;;
      --user) USER_NAME="$2"; shift 2 ;;
      --url) URL="$2"; shift 2 ;;
      *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
  done
  [ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
  [ -n "$TAGS" ] || { echo "--tags is required (yuno-prod for the deploy host, yuno-ci for a test host)" >&2; exit 2; }
  [ -n "${RUNNER_TOKEN:-}" ] || { echo "RUNNER_TOKEN is not set in the environment" >&2; exit 2; }
  id "$USER_NAME" >/dev/null 2>&1 || { echo "user $USER_NAME does not exist (run bootstrap-droplet.sh first)" >&2; exit 1; }
  export DEBIAN_FRONTEND=noninteractive

  if ! command -v gitlab-runner >/dev/null; then
    echo "== gitlab-runner package"
    curl -fsSL "https://packages.gitlab.com/runner/gitlab-runner/gpgkey" | gpg --dearmor -o /usr/share/keyrings/runner_gitlab-runner-archive-keyring.gpg
    # shellcheck disable=SC1091
    . /etc/os-release
    echo "deb [signed-by=/usr/share/keyrings/runner_gitlab-runner-archive-keyring.gpg] https://packages.gitlab.com/runner/gitlab-runner/ubuntu/ ${VERSION_CODENAME} main" \
      > /etc/apt/sources.list.d/runner_gitlab-runner.list
    apt-get update -q
    apt-get install -y -q gitlab-runner >/dev/null
  fi

  # The package installs the service as the gitlab-runner user; re-install it as the deploy user.
  HOME_DIR=$(getent passwd "$USER_NAME" | cut -d: -f6)
  # Ubuntu's default ~/.bash_logout runs clear_console, which fails without a terminal and
  # makes every job die in "prepare environment" (gitlab-runner docs: shell profile loading).
  if [ -s "$HOME_DIR/.bash_logout" ]; then
    : > "$HOME_DIR/.bash_logout"
    echo "== emptied $HOME_DIR/.bash_logout for non-interactive job shells"
  fi
  if ! grep -q -- "--user $USER_NAME" /etc/systemd/system/gitlab-runner.service 2>/dev/null; then
    echo "== service as $USER_NAME"
    gitlab-runner stop >/dev/null 2>&1 || true
    gitlab-runner uninstall >/dev/null 2>&1 || true
    gitlab-runner install --user "$USER_NAME" --working-directory "$HOME_DIR" >/dev/null
  fi

  # Tags of a runner created in GitLab's UI live in GitLab; the local config carries the token.
  CONFIG="/etc/gitlab-runner/config.toml"
  if [ -f "$CONFIG" ] && grep -qF -- "$RUNNER_TOKEN" "$CONFIG" 2>/dev/null; then
    echo "== this runner token is already registered here; skipping registration"
  else
    echo "== registering with $URL (tags: $TAGS)"
    # The token goes through the environment (CI_SERVER_TOKEN), not the command line.
    CI_SERVER_URL="$URL" CI_SERVER_TOKEN="$RUNNER_TOKEN" gitlab-runner register --non-interactive \
      --executor shell --description "$(hostname) ($TAGS)" >/dev/null
  fi
  gitlab-runner start >/dev/null 2>&1 || gitlab-runner restart >/dev/null
  gitlab-runner verify >/dev/null 2>&1 && echo "runner registered and verified" || echo "runner installed; verify reported a problem (gitlab-runner verify)" >&2
}
main "$@" </dev/null
