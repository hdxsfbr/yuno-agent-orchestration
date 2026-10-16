#!/usr/bin/env bash
# Creates the DigitalOcean droplet for a public Yuno deployment with doctl and waits until it
# answers on SSH. Prints the droplet's public IP last, on its own line, so callers can capture
# it. Idempotent: an existing droplet of that name is reused. Needs doctl authenticated here.
#
# Usage: scripts/deploy/create-droplet.sh [--name yuno-prod] [--size s-4vcpu-8gb] [--region sfo3]
#          [--image ubuntu-24-04-x64] [--ssh-key NAME_OR_ID_OR_FINGERPRINT] [--tag yuno] [--no-wait]
# Without --ssh-key every SSH key on the account is installed on the droplet.
set -euo pipefail

NAME=yuno-prod SIZE=s-4vcpu-8gb REGION=sfo3 IMAGE=ubuntu-24-04-x64 SSH_KEY="" TAG=yuno WAIT=1
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2 ;;
    --size) SIZE="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --ssh-key) SSH_KEY="$2"; shift 2 ;;
    --tag) TAG="$2"; shift 2 ;;
    --no-wait) WAIT=0; shift ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
command -v doctl >/dev/null || { echo "doctl not found (https://docs.digitalocean.com/reference/doctl/)" >&2; exit 1; }

if [ -z "$SSH_KEY" ]; then
  SSH_KEY=$(doctl compute ssh-key list --format ID --no-header | paste -sd, -)
elif ! [[ "$SSH_KEY" =~ ^[0-9]+$ || "$SSH_KEY" =~ : ]]; then
  # a key name: resolve it to its id
  SSH_KEY=$(doctl compute ssh-key list --format ID,Name --no-header | awk -v n="$SSH_KEY" '$2 == n { print $1 }')
fi
[ -n "$SSH_KEY" ] || { echo "no matching SSH key on the account (doctl compute ssh-key list)" >&2; exit 1; }

if doctl compute droplet list --format Name --no-header | grep -qx "$NAME"; then
  echo "droplet $NAME already exists; reusing it" >&2
else
  echo "creating $NAME ($SIZE, $REGION, $IMAGE) ..." >&2
  doctl compute droplet create "$NAME" --size "$SIZE" --region "$REGION" --image "$IMAGE" \
    --ssh-keys "$SSH_KEY" --tag-name "$TAG" --enable-monitoring --wait >&2
fi
IP=$(doctl compute droplet get "$NAME" --format PublicIPv4 --no-header)
[ -n "$IP" ] || { echo "droplet $NAME has no public IPv4 yet" >&2; exit 1; }

if [ "$WAIT" = 1 ]; then
  echo "waiting for ssh on $IP ..." >&2
  ok=0
  for _ in $(seq 1 60); do
    if ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new "root@$IP" true 2>/dev/null; then ok=1; break; fi
    sleep 5
  done
  [ "$ok" = 1 ] || { echo "ssh did not come up on $IP within 5 minutes" >&2; exit 1; }
fi
echo "$IP"
