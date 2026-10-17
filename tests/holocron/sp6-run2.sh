#!/usr/bin/env bash
# SP6 matrix 2: writer rename modes (posix semantics, ReplaceFileW) against the same readers. Run from /mnt/c/Windows/Temp.
# usage: sp6-run2.sh <sp6b.exe unix path> <C: dir (windows path)>
exe=$1; dir=$2
PS='powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Temp\holocron-sp\sp6-reader.ps1'
f="$dir\\status.json"
case_() { local title=$1 wargs=$2; shift 2
  echo "### $title"
  command rm -f /mnt/c/Windows/Temp/holocron-sp/sp6/status.json*
  "$exe" write -path "$f" -for 12s $wargs > /tmp/sp6.w.out 2>&1 &
  local w=$!; sleep 1.5
  "$@" < /dev/null 2>&1 | tr -d '\r'
  wait $w; tr -d '\r' < /tmp/sp6.w.out
}
for mode in posix replacefile; do
  case_ "[$mode] Go reader tight, FILE_SHARE_DELETE" "-rename $mode" "$exe" read -path "$f" -for 9s -open sd
  case_ "[$mode] Go reader tight, os.Open (no share-delete)" "-rename $mode" "$exe" read -path "$f" -for 9s -open go
  case_ "[$mode] Go reader holding 2 s, FILE_SHARE_DELETE" "-rename $mode" "$exe" read -path "$f" -for 9s -open sd -hold 2s
  case_ "[$mode] Go reader holding 2 s, os.Open" "-rename $mode" "$exe" read -path "$f" -for 9s -open go -hold 2s
  case_ "[$mode] PS FileStream ReadWrite|Delete holding 2 s" "-rename $mode" $PS -Path "$f" -Mode fs-rwd -Seconds 9 -HoldMs 2000
  case_ "[$mode] PS Get-Content tight" "-rename $mode" $PS -Path "$f" -Mode gc -Seconds 9
done
case_ "[posix] writer retry 10x100ms + Go reader holding 2 s os.Open" "-rename posix -retries 10 -retrydelay 100ms" "$exe" read -path "$f" -for 9s -open go -hold 2s
echo "### 9P share, posix and replacefile writers"
unc=$3; u="$unc\\status.json"
for mode in posix replacefile; do
  "$exe" write -path "$u" -for 6s -rename $mode > /tmp/sp6.w.out 2>&1 & w=$!; sleep 1.5
  "$exe" read -path "$u" -for 4s -open sd 2>&1 | tr -d '\r'; wait $w; echo "[$mode on 9P]"; tr -d '\r' < /tmp/sp6.w.out
done
