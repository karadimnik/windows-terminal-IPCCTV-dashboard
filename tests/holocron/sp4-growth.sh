#!/usr/bin/env bash
# SP4 growth check: 30 s segments, 50 s of recording; do size/mtime of the open file move while ffmpeg writes?
# usage: sp4-growth.sh linux <out> <ffmpeg> <sp4>  |  sp4-growth.sh windows <out-unix> <out-windows> <sp4.exe unix>
kind=$1; out=$2; mkdir -p "$out/cam"
ARGS='-hide_banner -loglevel warning -nostdin -re -f lavfi -i testsrc2=size=320x180:rate=10 -re -f lavfi -i sine=frequency=440:sample_rate=8000 -map 0:v:0 -map 1:a:0? -c:v libx264 -preset ultrafast -g 10 -c:a pcm_alaw -ar 8000 -ac 1 -t 50 -f segment -segment_time 30 -segment_atclocktime 1 -reset_timestamps 1 -strftime 1 -segment_format matroska'
if [ "$kind" = linux ]; then
  "$4" watch -dir "$out/cam" -safe Cam -seg 30 -every 3s -for 52s > "$out/watch.log" 2>&1 &
  $3 $ARGS "$out/cam/Cam_%Y%m%d_%H%M%S%z.mkv" 2> "$out/ff.err" < /dev/null
  wait
else
  win=$3
  "$4" watch -dir "$win\\cam" -safe Cam -seg 30 -every 3s -for 52s > "$out/watch.log" 2>&1 &
  /mnt/c/ffmpeg/bin/ffmpeg.exe $ARGS "$win\\cam\\Cam_%Y%m%d_%H%M%S%z.mkv" 2> "$out/ff.err" < /dev/null
  wait
fi
