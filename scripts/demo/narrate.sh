#!/usr/bin/env sh
# Voices scripts/demo/narration/NN.txt into NN.wav with piper (local text-to-speech) in a
# throwaway container; the voice model is cached under scripts/demo/.piper (git-ignored).
# Usage: scripts/demo/narrate.sh [VOICE]   (default en_US-lessac-medium)
set -eu
VOICE=${1:-en_US-lessac-medium}
DIR=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$DIR/.piper"
docker run --rm -v "$DIR/narration:/narration" -v "$DIR/.piper:/voices" --entrypoint sh python:3.12-slim -c '
  pip install -q --no-cache-dir piper-tts >/dev/null 2>&1
  [ -f /voices/'"$VOICE"'.onnx ] || python -m piper.download_voices '"$VOICE"' --data-dir /voices >/dev/null 2>&1
  for f in /narration/*.txt; do
    n=$(basename "$f" .txt)
    python -m piper --model /voices/'"$VOICE"'.onnx --output_file "/narration/$n.wav" < "$f" >/dev/null 2>&1 && echo "voiced $n"
  done
  chown -R '"$(id -u):$(id -g)"' /narration /voices
'
