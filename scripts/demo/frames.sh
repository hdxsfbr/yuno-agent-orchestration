#!/usr/bin/env bash
# Extracts still frames from a recording so it can be checked without a player.
#   scripts/demo/frames.sh 02.webm 1 5 10        frames at 1 s, 5 s and 10 s -> out/frames/02-1.png ...
#   scripts/demo/frames.sh demo.mp4 0.5 60 120   works on the stitched video too
#   FRAMES_EVERY=10 scripts/demo/frames.sh 04.webm   one frame every 10 s
# ffmpeg runs in a container (FFMPEG_IMAGE, default linuxserver/ffmpeg); files are read from OUT_DIR.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="$(cd "${OUT_DIR:-$here/out}" && pwd)"
file="${1:?usage: frames.sh <file in out/> [seconds...]}"; shift
[[ -f "$out/$file" ]] || { echo "$out/$file not found" >&2; exit 1; }
mkdir -p "$out/frames"
if [[ $# -eq 0 ]]; then
  every="${FRAMES_EVERY:-10}"
  docker run --rm --user "$(id -u):$(id -g)" -v "$out:/out" --entrypoint ffmpeg "${FFMPEG_IMAGE:-linuxserver/ffmpeg:latest}" \
    -nostdin -hide_banner -loglevel error -y -i "/out/$file" -vf "fps=1/$every" "/out/frames/${file%.*}-%03d.png" < /dev/null
else
  for t in "$@"; do
    docker run --rm --user "$(id -u):$(id -g)" -v "$out:/out" --entrypoint ffmpeg "${FFMPEG_IMAGE:-linuxserver/ffmpeg:latest}" \
      -nostdin -hide_banner -loglevel error -y -ss "$t" -i "/out/$file" -frames:v 1 "/out/frames/${file%.*}-$t.png" < /dev/null
  done
fi
ls "$out/frames" | sed 's|^|out/frames/|'
