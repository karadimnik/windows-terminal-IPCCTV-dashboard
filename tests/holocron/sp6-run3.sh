#!/usr/bin/env bash
# SP6 matrix 3: ReplaceFileW (with the first-write fallback) and writer-only baselines. Run from /mnt/c/Windows/Temp.
# usage: sp6-run3.sh <sp6c.exe unix path> <C: dir (windows path)> <UNC dir>
exe=$1; dir=$2; unc=$3
PS='powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Temp\holocron-sp\sp6-reader.ps1'
f="$dir\\status.json"
case_() { local title=$1 wargs=$2; shift 2
  echo "### $title"; command rm -f /mnt/c/Windows/Temp/holocron-sp/sp6/status.json*
  "$exe" write -path "$f" -for 10s $wargs > /tmp/sp6.w.out 2>&1 & local w=$!; sleep 1.5
  "$@" < /dev/null 2>&1 | tr -d '\r'; wait $w; tr -d '\r' < /tmp/sp6.w.out; }
case_ "[replacefile] Go reader tight, FILE_SHARE_DELETE" "-rename replacefile" "$exe" read -path "$f" -for 7s -open sd
case_ "[replacefile] Go reader tight, os.Open" "-rename replacefile" "$exe" read -path "$f" -for 7s -open go
case_ "[replacefile] Go reader holding 2 s, FILE_SHARE_DELETE" "-rename replacefile" "$exe" read -path "$f" -for 7s -open sd -hold 2s
case_ "[replacefile] PS Get-Content tight" "-rename replacefile" $PS -Path "$f" -Mode gc -Seconds 7
case_ "[mfe] writer only, no reader (C:)" "" true
case_ "[posix] writer only, no reader (C:)" "-rename posix" true
u="$unc\\status.json"
for mode in mfe; do
  echo "### [$mode] writer only on 9P"; "$exe" write -path "$u" -for 6s -rename $mode 2>&1 | tr -d '\r'
done
