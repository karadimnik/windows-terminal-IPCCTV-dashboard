#!/usr/bin/env bash
# SP1: run the segment-muxer argument vector (IR 1.1) against lavfi sources.
# usage: sp1-run.sh <linux|windows> <ffmpeg-binary> <out-root(unix path)> [win-root(windows path of out-root, windows only)]
# Runs 4 TZ values x 2 segment lengths in parallel; each case is its own folder. -re paces the lavfi sources in real time
# (the only addition to the product's vector besides the lavfi inputs and -t).
set -u
kind=$1; ff=$2; out=$3; win=${4:-}
mkdir -p "$out"
export WSLENV=TZ
declare -A tzs=( [utc]=UTC [buc]=Europe/Bucharest [kol]=Asia/Kolkata [stj]=America/St_Johns )
[ -n "${SP1_TZMODE:-}" ] && [ "$SP1_TZMODE" = posix ] && tzs=( [utc]=UTC0 [buc]=EET-2EEST [kol]=IST-5:30 [stj]=NST3:30NDT )
pids=()
for key in utc buc kol stj; do
  for seg in 10 7; do
    case=${key}_s${seg}
    mkdir -p "$out/$case"
    if [ "$kind" = windows ]; then pat="$win\\$case\\Test_%Y%m%d_%H%M%S%z.mkv"; else pat="$out/$case/Test_%Y%m%d_%H%M%S%z.mkv"; fi
    t=$(( seg == 10 ? 45 : 35 ))
    (
      date -u +%s.%N > "$out/$case.start"
      TZ=${tzs[$key]} "$ff" -hide_banner -loglevel warning -nostdin \
        -re -f lavfi -i testsrc2=size=320x180:rate=10 -re -f lavfi -i sine=frequency=440:sample_rate=8000 \
        -map 0:v:0 -map '1:a:0?' -c:v libx264 -preset ultrafast -g 10 -c:a pcm_alaw -ar 8000 -ac 1 -t $t \
        -f segment -segment_time $seg -segment_atclocktime 1 -reset_timestamps 1 -strftime 1 -segment_format matroska \
        "$pat" > "$out/$case.stdout" 2> "$out/$case.stderr"
      echo $? > "$out/$case.exit"
      date -u +%s.%N > "$out/$case.end"
    ) &
    pids+=($!)
  done
done
wait "${pids[@]}"
echo done
