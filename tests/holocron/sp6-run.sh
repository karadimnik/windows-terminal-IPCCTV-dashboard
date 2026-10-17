#!/usr/bin/env bash
# SP6 experiment matrix. Run from /mnt/c/Windows/Temp. usage: sp6-run.sh <sp6.exe unix path> <C: dir (windows path)> <UNC dir for the 9P case>
exe=$1; dir=$2; unc=$3
PS='powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Temp\holocron-sp\sp6-reader.ps1'
f="$dir\\status.json"
case_() { # title, writer-args, reader-command...
  local title=$1 wargs=$2; shift 2
  echo "### $title"
  command rm -f "/mnt/c/Windows/Temp/holocron-sp/sp6/status.json"*
  "$exe" write -path "$f" -for 12s $wargs > /tmp/sp6.w.out 2>&1 &
  local w=$!; sleep 1.5
  "$@" < /dev/null 2>&1 | tr -d '\r'
  wait $w; cat /tmp/sp6.w.out | tr -d '\r'
}
case_ "E1 writer + Go goroutine reader (tight, os.ReadFile-like open)" "-selfread" true
case_ "E2 writer + Go process reader tight (os.Open)" "" "$exe" read -path "$f" -for 9s -open go
case_ "E2b writer + Go process reader tight (FILE_SHARE_DELETE)" "" "$exe" read -path "$f" -for 9s -open sd
case_ "E3a writer + PS Get-Content -Raw tight" "" $PS -Path "$f" -Mode gc -Seconds 9
case_ "E3b writer + PS [IO.File]::OpenRead tight (FileShare.Read)" "" $PS -Path "$f" -Mode openread -Seconds 9
case_ "E3c writer + PS FileStream ReadWrite tight" "" $PS -Path "$f" -Mode fs-rw -Seconds 9
case_ "E3d writer + PS FileStream ReadWrite|Delete tight" "" $PS -Path "$f" -Mode fs-rwd -Seconds 9
case_ "E4 writer + Go reader holding 2 s (os.Open)" "" "$exe" read -path "$f" -for 9s -open go -hold 2s
case_ "E5 writer + Go reader holding 2 s (FILE_SHARE_DELETE)" "" "$exe" read -path "$f" -for 9s -open sd -hold 2s
case_ "E6 writer + PS FileStream ReadWrite holding 2 s" "" $PS -Path "$f" -Mode fs-rw -Seconds 9 -HoldMs 2000
case_ "E7 writer + PS FileStream ReadWrite|Delete holding 2 s" "" $PS -Path "$f" -Mode fs-rwd -Seconds 9 -HoldMs 2000
case_ "E7b writer + PS OpenRead (FileShare.Read) holding 2 s" "" $PS -Path "$f" -Mode openread -Seconds 9 -HoldMs 2000
case_ "E8 writer with 10 x 100 ms retries + Go reader holding 2 s (os.Open)" "-retries 10 -retrydelay 100ms" "$exe" read -path "$f" -for 9s -open go -hold 2s
case_ "E8b writer with 30 x 100 ms retries + Go reader holding 2 s (os.Open)" "-retries 30 -retrydelay 100ms" "$exe" read -path "$f" -for 9s -open go -hold 2s
echo "### E9 writer on the 9P share $unc"
mkdir -p "/tmp/claude-1000/-home-nikolas/2d5e9350-273d-42c1-aa11-bbbff02c8dbc/scratchpad/sp6share"
u="$unc\\status.json"
"$exe" write -path "$u" -for 8s > /tmp/sp6.w.out 2>&1 &
w=$!; sleep 1.5; "$exe" read -path "$u" -for 6s -open go 2>&1 | tr -d '\r'; wait $w; tr -d '\r' < /tmp/sp6.w.out
echo "### E9b 9P share, reader holding 2 s (os.Open)"
"$exe" write -path "$u" -for 8s > /tmp/sp6.w.out 2>&1 &
w=$!; sleep 1.5; "$exe" read -path "$u" -for 6s -open go -hold 2s 2>&1 | tr -d '\r'; wait $w; tr -d '\r' < /tmp/sp6.w.out
