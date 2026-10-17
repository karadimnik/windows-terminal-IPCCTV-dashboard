#!/usr/bin/env bash
# SP1b (Linux): try ways to hand ffmpeg an RTSP credential, against the Basic-auth stub (spikes/sp1b).
# usage: sp1b-linux.sh <ffmpeg> <stub-binary> <workdir>
ff=$1; stub=$2; w=$3; mkdir -p "$w"; cd "$w" || exit 1
U='rtsp://fakeuser:fakepass@127.0.0.1:18555/t'
printf '%s\n' "$U" > url.txt; chmod 600 url.txt
printf "file '%s'\n" "$U" > list.txt
mkdir -p home; printf 'machine 127.0.0.1 login fakeuser password fakepass\n' > home/.netrc; chmod 600 home/.netrc
try() { # name, env-prefix, args...
  local name=$1; shift
  "$stub" -addr 127.0.0.1:18555 -life 12s -hold 4s > "$w/stub.$name.log" 2>&1 & local sp=$!
  sleep 0.5
  env "$@" > "$w/ff.$name.out" 2>&1 < "$w/stdin.${name%%_*}" & local fp=$!
  sleep 2
  local cl; cl=$(tr '\0' ' ' < /proc/$fp/cmdline 2>/dev/null)
  local leaked=no; case "$cl" in *fakepass*) leaked=YES;; esac
  wait $fp; local rc=$?; kill $sp 2>/dev/null; wait $sp 2>/dev/null
  local auth; auth=$(grep -c 'user_ok=true pass_ok=true' "$w/stub.$name.log")
  echo "$name | authenticated=$([ "$auth" -gt 0 ] && echo YES || echo no) | fakepass_in_cmdline=$leaked | rc=$rc | cmdline: ${cl:0:150}"
  tail -c 160 "$w/ff.$name.out" | tr '\n' '|'; echo
}
: > stdin.M0; : > stdin.M1; : > stdin.M2; : > stdin.M3; : > stdin.M4; printf '%s\n' "$U" > stdin.M5
try M0_url_in_argv      "$ff" -nostdin -loglevel warning -rtsp_transport tcp -i "$U" -t 3 -f null -
try M1_slash_i_file     "$ff" -nostdin -loglevel warning -rtsp_transport tcp -/i url.txt -t 3 -f null -
try M2_concat_file      "$ff" -nostdin -loglevel warning -f concat -safe 0 -protocol_whitelist file,rtsp,tcp,rtp,udp -i list.txt -t 3 -f null -
try M3_env_vars         RTSP_USER=fakeuser RTSP_PASSWORD=fakepass USER=fakeuser PASSWORD=fakepass "$ff" -nostdin -loglevel warning -rtsp_transport tcp -i rtsp://127.0.0.1:18555/t -t 3 -f null -
try M4_netrc            HOME="$w/home" "$ff" -nostdin -loglevel warning -rtsp_transport tcp -i rtsp://127.0.0.1:18555/t -t 3 -f null -
try M5_stdin_pipe       "$ff" -loglevel warning -i pipe:0 -t 3 -f null -
