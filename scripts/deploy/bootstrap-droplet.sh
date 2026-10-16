#!/usr/bin/env bash
# Prepares a fresh Ubuntu 24.04 host for the Yuno stack. Run as root on the host, fed over ssh:
#   ssh root@HOST bash -s -- [--repo URL] [--ref main] [--user yuno] [--swap-gb 4] < scripts/deploy/bootstrap-droplet.sh
# Installs Docker Engine (if missing), git and unattended upgrades; creates the deploy user
# (uid 1000 so the bind mounts match the containers' user, in the docker group, with root's
# authorized keys); adds a swap file; opens only 22, 80 and 443 in ufw; clones the repository.
# Idempotent: safe to run again.
set -euo pipefail

REPO=https://labs.gauntletai.com/andrebatista/yuno-agent-orchestration.git
REF=main USER_NAME=yuno SWAP_GB=4
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    --user) USER_NAME="$2"; shift 2 ;;
    --swap-gb) SWAP_GB="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive

echo "== packages"
apt-get update -q
apt-get install -y -q ca-certificates curl git ufw unattended-upgrades >/dev/null
if ! command -v docker >/dev/null; then
  echo "== docker engine"
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  # shellcheck disable=SC1091
  . /etc/os-release
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update -q
  apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
fi
systemctl enable --now docker >/dev/null
docker compose version >/dev/null || { echo "docker compose v2 is missing" >&2; exit 1; }

echo "== deploy user $USER_NAME"
if ! id "$USER_NAME" >/dev/null 2>&1; then
  if getent passwd 1000 >/dev/null; then
    useradd -m -s /bin/bash "$USER_NAME"
    echo "warning: uid 1000 is taken; chown the data directory to the container user by hand (docs/deploy.md)" >&2
  else
    useradd -m -s /bin/bash -u 1000 "$USER_NAME"
  fi
fi
usermod -aG docker "$USER_NAME"
HOME_DIR=$(getent passwd "$USER_NAME" | cut -d: -f6)
install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$HOME_DIR/.ssh"
if [ -f /root/.ssh/authorized_keys ]; then
  install -m 0600 -o "$USER_NAME" -g "$USER_NAME" /root/.ssh/authorized_keys "$HOME_DIR/.ssh/authorized_keys"
fi

if [ "$SWAP_GB" -gt 0 ] && ! swapon --show --noheadings | grep -q '^/swapfile'; then
  echo "== swap ${SWAP_GB}G"
  fallocate -l "${SWAP_GB}G" /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo 'vm.swappiness=10' > /etc/sysctl.d/90-yuno.conf
  sysctl -q -w vm.swappiness=10
fi

echo "== firewall (22, 80, 443; Docker publishes past ufw, so the stack binds the rest to loopback)"
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443 >/dev/null
ufw --force enable >/dev/null

echo "== repository"
DIR="$HOME_DIR/yuno-agent-orchestration"
if [ ! -d "$DIR/.git" ]; then
  sudo -u "$USER_NAME" git clone -q "$REPO" "$DIR"
fi
sudo -u "$USER_NAME" git -C "$DIR" fetch -q origin
if sudo -u "$USER_NAME" git -C "$DIR" show-ref -q --verify "refs/remotes/origin/$REF"; then
  sudo -u "$USER_NAME" git -C "$DIR" checkout -q -B "$REF" "origin/$REF"
else
  sudo -u "$USER_NAME" git -C "$DIR" checkout -q "$REF"
fi
echo "bootstrap done: user $USER_NAME, repo $DIR at $(git -C "$DIR" rev-parse --short HEAD)"
echo "next: scripts/deploy/deploy.sh --host <this host> --env-file <.env for this host> (docs/deploy.md)"
