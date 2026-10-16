#!/usr/bin/env bash
# Turns the recorded scenes into one video. ffmpeg runs in a container; nothing is installed on the host.
#
#   per scene   strip the cuts.json intervals (keeping the click and about 1 s of the pending state, and
#               the moment just before the result), re-encode to one uniform intermediate (1440x900, 30 fps,
#               yuv420p, H.264/AAC), and mux narration/NN.(mp3|wav|m4a|ogg|flac) when it exists
#   narration   a still is held for its clip plus a breath; footage longer than clip plus a couple of seconds
#               is sped up to fit (capped); shorter footage holds its last frame
#   then        concatenate the intermediates into demo.mp4 and write title.png and thumbnail.png
#
# Usage: scripts/demo/stitch.sh            (also: make demo-stitch)
# Knobs (environment): OUT_DIR, NARRATION_DIR, FFMPEG_IMAGE, FPS, WIDTH, HEIGHT, CRF, CUT_HEAD_MS, CUT_TAIL_MS,
#   MIN_CUT_MS, MAX_SPEEDUP, TAIL_SLACK, BREATH
set -euo pipefail

if [[ -z "${STITCH_INSIDE:-}" ]]; then
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  out="${OUT_DIR:-$here/out}"
  [[ -d "$out" ]] || { echo "no footage: $out does not exist (run make demo-record first)" >&2; exit 1; }
  out="$(cd "$out" && pwd)"
  env_args=()
  for v in FPS WIDTH HEIGHT CRF CUT_HEAD_MS CUT_TAIL_MS MIN_CUT_MS MAX_SPEEDUP TAIL_SLACK BREATH; do
    if [[ -n "${!v:-}" ]]; then env_args+=(-e "$v=${!v}"); fi
  done
  exec docker run --rm --user "$(id -u):$(id -g)" --entrypoint bash \
    -v "$out:/out" -v "$(cd "${NARRATION_DIR:-$here/narration}" && pwd):/narration:ro" -v "$here/stitch.sh:/stitch.sh:ro" \
    -e STITCH_INSIDE=1 "${env_args[@]}" "${FFMPEG_IMAGE:-linuxserver/ffmpeg:latest}" /stitch.sh < /dev/null
fi

# ---- inside the container -----------------------------------------------------------------------------

FPS="${FPS:-30}"; W="${WIDTH:-1440}"; H="${HEIGHT:-900}"; CRF="${CRF:-20}"
CUT_HEAD_MS="${CUT_HEAD_MS:-1000}"   # kept from the start of a wait: the click and its pending state
CUT_TAIL_MS="${CUT_TAIL_MS:-600}"    # kept from the end of a wait: the moment before the result
MIN_CUT_MS="${MIN_CUT_MS:-1200}"     # a wait whose strippable part is shorter than this is left alone
MAX_SPEEDUP="${MAX_SPEEDUP:-1.6}"    # cap on time-compression of footage that outruns its narration
TAIL_SLACK="${TAIL_SLACK:-2}"        # footage may run this long past the narration before it is sped up
BREATH="${BREATH:-1}"                # silence after the narration ends

cd /out
work=.stitch
rm -rf "$work"; mkdir -p "$work"
[[ -f cuts.json ]] || echo '[]' > cuts.json

ff()  { ffmpeg -nostdin -hide_banner -loglevel error -y "$@"; }
num() { awk "BEGIN { printf \"%.3f\", $1 }"; }
dur() {
  local d
  d="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$1" 2>/dev/null || true)"
  if [[ ! "$d" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then   # a WebM written without a duration: read it by decoding
    d="$(ffmpeg -nostdin -i "$1" -f null - 2>&1 | grep -o 'time=[0-9:.]*' | tail -1 | cut -d= -f2 | awk -F: '{ print $1*3600 + $2*60 + $3 }')"
  fi
  echo "$d"
}

ENC=(-c:v libx264 -preset medium -crf "$CRF" -pix_fmt yuv420p -r "$FPS" -fps_mode cfr -g $((FPS * 2))
     -c:a aac -b:a 160k -ar 48000 -ac 2 -video_track_timescale $((FPS * 1000)))
FIT="scale=${W}:${H}:force_original_aspect_ratio=decrease,pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2:color=white,setsar=1,format=yuv420p"

# Intervals to strip for one scene, in ms, "a b" per line, merged, from cuts.json.
strips() {
  jq -r --arg s "$1" --argjson head "$CUT_HEAD_MS" --argjson tail "$CUT_TAIL_MS" --argjson min "$MIN_CUT_MS" '
    [ .[] | select(.scene == $s)
      | { a: (.start_ms + (.keep_head_ms // $head)), b: (.end_ms - (.keep_tail_ms // $tail)), explicit: has("keep_head_ms") }
      | select(.b - .a >= (if .explicit then 1 else $min end)) ]
    | sort_by(.a)
    | reduce .[] as $c ([]; if length > 0 and $c.a <= .[-1].b then (.[-1].b |= ([., $c.b] | max)) else . + [{a: $c.a, b: $c.b}] end)
    | .[] | "\(.a) \(.b)"' cuts.json
}

ids="$(ls | sed -n 's/^\([0-9][0-9]\)\.\(webm\|png\)$/\1/p' | sort -u)"
[[ -n "$ids" ]] || { echo "nothing to stitch in /out: expected NN.webm or NN.png" >&2; exit 1; }

printf '\n%-5s %-6s %9s %9s %9s %7s %9s  %s\n' scene source footage cut narr speed out narration
: > "$work/list.txt"
first_video=""

for id in $ids; do
  audio=""
  for ext in mp3 wav m4a aac ogg opus flac; do
    [[ -f "/narration/$id.$ext" ]] && { audio="/narration/$id.$ext"; break; }
  done
  narr=""; [[ -n "$audio" ]] && narr="$(dur "$audio")"
  # Narration is input 1; scenes without a clip get silence, so every intermediate has the same streams.
  if [[ -n "$audio" ]]; then a_in=(-i "$audio"); else a_in=(-f lavfi -i "anullsrc=r=48000:cl=stereo"); fi
  a_fc="[1:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,apad[a]"
  seg="$work/$id.mp4"

  if [[ -f "$id.webm" ]]; then
    d="$(dur "$id.webm")"
    [[ -n "$d" ]] || { echo "cannot read the length of $id.webm; re-record scene $id" >&2; exit 1; }
    sel=""; cut_total=0; terms=()
    while read -r a b; do
      [[ -n "${a:-}" ]] || continue
      as="$(num "$a / 1000")"; bs="$(num "$b / 1000")"
      awk "BEGIN { exit !($as < $d) }" || continue            # a strip past the end of the footage is moot
      awk "BEGIN { exit !($bs > $d) }" && bs="$d"
      terms+=("between(t,$as,$bs)")
      cut_total="$(num "$cut_total + $bs - $as")"
    done < <(strips "$id")
    if ((${#terms[@]} > 0)); then sel="select='not($(IFS=+; echo "${terms[*]}"))',"; fi
    f="$(num "$d - $cut_total")"                              # footage that remains

    factor=1; out="$f"
    if [[ -n "$narr" ]]; then
      target="$(num "$narr + $TAIL_SLACK")"
      factor="$(awk "BEGIN { r = $f / $target; if (r < 1) r = 1; if (r > $MAX_SPEEDUP) r = $MAX_SPEEDUP; printf \"%.3f\", r }")"
      out="$(awk "BEGIN { f2 = $f / $factor; m = $narr + $BREATH; printf \"%.3f\", (f2 > m ? f2 : m) }")"
    fi
    f2="$(num "$f / $factor")"
    pad="$(num "$out - $f2")"
    v_fc="[0:v]setpts=PTS-STARTPTS,fps=${FPS},${sel}setpts=N/(${FPS}*${factor}*TB),fps=${FPS},${FIT},tpad=stop_mode=clone:stop_duration=${pad}[v]"
    ff -i "$id.webm" "${a_in[@]}" -filter_complex "$v_fc;$a_fc" -map "[v]" -map "[a]" -t "$out" "${ENC[@]}" "$seg"
    [[ -n "$first_video" ]] || first_video="$id"
    printf '%-5s %-6s %8.1fs %8.1fs %9s %6.2fx %8.1fs  %s\n' "$id" video "$d" "$cut_total" "${narr:+$(num "$narr")s}" "$factor" "$out" "${audio:+$(basename "$audio")}"
  else
    seconds="$(tr -d "[:space:]" 2>/dev/null < "$id.seconds" || true)"; seconds="${seconds:-5}"
    out="$seconds"
    [[ -n "$narr" ]] && out="$(num "$narr + $BREATH")"        # a still is held for its clip plus a breath
    ff -loop 1 -framerate "$FPS" -i "$id.png" "${a_in[@]}" -filter_complex "[0:v]${FIT}[v];$a_fc" \
       -map "[v]" -map "[a]" -t "$out" "${ENC[@]}" "$seg"
    printf '%-5s %-6s %9s %9s %9s %7s %8.1fs  %s\n' "$id" still - - "${narr:+$(num "$narr")s}" - "$out" "${audio:+$(basename "$audio")}"
  fi
  echo "file '$id.mp4'" >> "$work/list.txt"
done

ff -f concat -safe 0 -i "$work/list.txt" -c copy -movflags +faststart demo.mp4
total="$(dur demo.mp4)"

# Title frame and thumbnail.
if [[ -f 01.png ]]; then cp 01.png title.png; else ff -ss 0.5 -i demo.mp4 -frames:v 1 title.png; fi
if [[ -f thumbnail-src.png ]]; then
  ff -i thumbnail-src.png -vf "scale=1280:800,crop=1280:720" -frames:v 1 thumbnail.png
else
  pick="${first_video:-$(echo "$ids" | head -1)}"
  ff -ss "$(num "$(dur "$work/$pick.mp4") * 0.3")" -i "$work/$pick.mp4" -vf "scale=1280:800,crop=1280:720" -frames:v 1 thumbnail.png
fi

m=$(awk "BEGIN { printf \"%d\", $total / 60 }"); s=$(awk "BEGIN { printf \"%.1f\", $total - 60 * int($total / 60) }")
echo
echo "demo.mp4  ${m}m ${s}s  (${total} s)  $(ffprobe -v error -show_entries stream=codec_name,width,height,r_frame_rate -of csv=p=0 demo.mp4 | tr '\n' ' ')"
echo "title.png, thumbnail.png written next to it in $(pwd)"
