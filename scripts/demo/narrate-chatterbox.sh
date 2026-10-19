#!/usr/bin/env bash
# Voices scripts/demo/narration/NN.txt into NN.wav with the standard Chatterbox model and the
# operator's own reference voice, using the existing local setup in ~/code/chatterbox-play (its conda
# env `chatterbox`, GPU). The reference voice is the operator's own recording, used with permission.
# Usage: scripts/demo/narrate-chatterbox.sh [--style calm|documentary|balanced] [NN ...]
#   no scene ids: every narration/*.txt. Existing .wav files are overwritten.
set -eu
CB="${CHATTERBOX_ROOT:-$HOME/code/chatterbox-play}"
DIR="$(cd "$(dirname "$0")" && pwd)/narration"
style="documentary"
if [ "${1:-}" = "--style" ]; then style="$2"; shift 2; fi
ids=("$@")
if [ ${#ids[@]} -eq 0 ]; then for f in "$DIR"/*.txt; do ids+=("$(basename "$f" .txt)"); done; fi
for n in "${ids[@]}"; do
  conda run --no-capture-output -n chatterbox python "$CB/scripts/chatterbox_agent.py" \
    --text-file "$DIR/$n.txt" --out "$DIR/$n.wav" --style "$style" \
    --split sentence --silence-ms 250 --normalize < /dev/null
  # One sentence per generation: the model stops at about 40 s of audio, which cuts a long paragraph off.
  mv -f "$DIR/${n}_norm.wav" "$DIR/$n.wav"
  echo "voiced $n"
done
