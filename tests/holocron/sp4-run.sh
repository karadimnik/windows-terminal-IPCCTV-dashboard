#!/usr/bin/env bash
# SP4 scenario: record (lavfi, 5 s segments), hard-kill at t=22, restart at t=32 (camera drop), hard-kill at t=52, watch until the last file ages out.
# usage: sp4-run.sh linux <out-dir> <ffmpeg> <sp4-binary>      |   sp4-run.sh windows <out-dir-unix> <out-dir-windows> <sp4.exe unix path>
kind=$1; out=$2
mkdir -p "$out/cam"
ARGS='-hide_banner -loglevel warning -nostdin -re -f lavfi -i testsrc2=size=320x180:rate=10 -re -f lavfi -i sine=frequency=440:sample_rate=8000 -map 0:v:0 -map 1:a:0? -c:v libx264 -preset ultrafast -g 10 -c:a pcm_alaw -ar 8000 -ac 1 -f segment -segment_time 5 -segment_atclocktime 1 -reset_timestamps 1 -strftime 1 -segment_format matroska'
ts() { date +%H:%M:%S; }
if [ "$kind" = linux ]; then
  ff=$3; sp4=$4
  "$sp4" watch -dir "$out/cam" -safe Cam -seg 5 -every 2s -for 125s > "$out/watch.log" 2>&1 &
  W=$!; sleep 2
  echo "$(ts) start A"; $ff $ARGS "$out/cam/Cam_%Y%m%d_%H%M%S%z.mkv" 2> "$out/A.err" < /dev/null & A=$!
  sleep 20; echo "$(ts) kill -9 A"; kill -9 $A; wait $A 2>/dev/null
  sleep 10; echo "$(ts) start B"; $ff $ARGS "$out/cam/Cam_%Y%m%d_%H%M%S%z.mkv" 2> "$out/B.err" < /dev/null & B=$!
  sleep 20; echo "$(ts) kill -9 B"; kill -9 $B; wait $B 2>/dev/null
  wait $W
else
  win=$3; sp4=$4
  "$sp4" watch -dir "$win\\cam" -safe Cam -seg 5 -every 2s -for 125s > "$out/watch.log" 2>&1 &
  W=$!; sleep 2
  start() { command rm -f "$out/$1.pid"; powershell.exe -NoProfile -ExecutionPolicy Bypass -File 'C:\Windows\Temp\holocron-sp\sp-start.ps1' -Exe 'C:\ffmpeg\bin\ffmpeg.exe' -ArgLine "$ARGS $win\\cam\\Cam_%Y%m%d_%H%M%S%z.mkv" -Out "$win\\$1" > /dev/null 2>&1 < /dev/null & while [ ! -s "$out/$1.pid" ]; do sleep 0.2; done; cat "$out/$1.pid"; }
  A=$(start A); echo "$(ts) started A pid $A"
  sleep 20; powershell.exe -NoProfile -Command "Stop-Process -Id $A -Force" < /dev/null; echo "$(ts) killed A"
  sleep 10; B=$(start B); echo "$(ts) started B pid $B"
  sleep 20; powershell.exe -NoProfile -Command "Stop-Process -Id $B -Force" < /dev/null; echo "$(ts) killed B"
  wait $W
fi
echo "$(ts) done"
