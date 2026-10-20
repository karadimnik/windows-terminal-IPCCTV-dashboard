#!/usr/bin/env bash
# SP3 driver (Linux/WSL): one case per call. kill -9 the supervisor, watch the snapshot of its process tree for 5 s (zombies count as dead),
# kill survivors by pid. Only pids started by this script are ever signalled. Usage: sp3-lin.sh <case> [sp3 binary] [base dir]
set -u
CASE=$1; BIN=${2:-/tmp/holocron-sp3/sp3}; BASE=${3:-/tmp/holocron-sp3}
DIR=$BASE/$CASE; command rm -rf "$DIR"; mkdir -p "$DIR"
now() { date +%s%3N; }
st() { local l; l=$(cat /proc/$1/stat 2>/dev/null) || { echo gone; return; }; l=${l##*) }; echo "${l:0:1}"; }
alive() { local s; s=$(st "$1"); [ "$s" != gone ] && [ "$s" != Z ] && [ "$s" != X ]; }
tree() { local p; for p in $(ps -eo pid=,ppid= | awk -v P="$1" '$2==P{print $1}'); do echo "$p"; tree "$p"; done; }
declare -a SUP; SUP=("$BIN" supervise -dir "$DIR" -tag "$CASE")
KILLSIG=KILL; SETTLE=1.5
case $CASE in
  l1) SUP+=(-n 3 -mode locked) ;;
  l2) SUP+=(-n 3 -mode unlocked) ;;
  l3) SUP+=(-n 3 -mode lockexit) ;;
  l3b) SUP+=(-n 3 -mode lockexitfree) ;;
  l4) SUP+=(-n 3 -mode lockexitunlock) ;;
  l5) SUP+=(-n 3 -mode none) ;;
  l6) SUP+=(-n 3 -mode locked -kind grand) ;;
  l7) SUP+=(-n 3 -mode locked -kind grand); KILLSIG=TERM ;;
  l8) SUP+=(-n 3 -mode locked -short 1 -shortms 1500); SETTLE=0 ;;
  l9) SUP+=(-n 3 -mode none -reg) ;;
  l10) SUP+=(-n 3 -mode none -wrapper) ;;
  *) echo "unknown case"; exit 2 ;;
esac
"${SUP[@]}" >"$DIR/sup.out" 2>"$DIR/sup.err" &
SPID=$!
for _ in $(seq 1 300); do [ -s "$DIR/$CASE.state.json" ] && break; sleep 0.05; done
[ -s "$DIR/$CASE.state.json" ] || { echo "no state file"; kill -9 $SPID; exit 1; }
echo "CASE $CASE supervisor pid $SPID  $(cat "$DIR/$CASE.state.json")"
sleep $SETTLE
if [ "$CASE" = l3 ] || [ "$CASE" = l3b ] || [ "$CASE" = l4 ]; then
  echo "--- supervisor alive; children state 1 s after start (before any kill):"
  for p in $(tree $SPID); do echo "  pid $p ($(cat /proc/$p/comm 2>/dev/null)) state $(st $p)"; done
  grep -h "exited" "$DIR/$CASE.sup.log" | sed 's/^/  sup.log: /'
fi
PIDS=$(tree $SPID)
echo "snapshot: supervisor $SPID + $(for p in $PIDS; do echo -n "$p($(cat /proc/$p/comm 2>/dev/null)) "; done)"
if [ "$CASE" = l8 ]; then
  lf=$(ls "$DIR"/leaf-*.log | head -1); T=$(( $(head -1 "$lf" | cut -d' ' -f1) + 1500 ))
  while [ "$(now)" -lt "$T" ]; do :; done
fi
declare -A DEAD
T0=$(now)
kill -$KILLSIG $SPID
for _ in $(seq 1 500); do
  for p in $SPID $PIDS; do [ -z "${DEAD[$p]:-}" ] && ! alive "$p" && DEAD[$p]=$(( $(now) - T0 )); done
  sleep 0.01
done
wait $SPID 2>/dev/null; echo "supervisor exit status after $KILLSIG: $?"
SURV=""
echo "--- 5 s after kill -$KILLSIG of the supervisor:"
for p in $SPID $PIDS; do
  if [ -n "${DEAD[$p]:-}" ]; then echo "  pid $p died +${DEAD[$p]} ms"; else echo "  pid $p SURVIVED"; SURV="$SURV $p"; fi
done
echo "RESULT $CASE : $(echo $SURV | wc -w) survivors"
for f in "$DIR"/*.log; do echo "--- $(basename "$f"):"; while read -r e m; do case $e in 1[0-9]*) echo "$(printf '%8d' $(( e - T0 )))  $m";; *) echo "$e $m";; esac; done < "$f"; done
if [ "$CASE" = l9 ]; then
  echo "=== registry reap by a NEW supervisor ==="
  /usr/bin/sleep 600 & DECOY=$!
  echo "decoy (unrelated sleep I started, recorded) pid $DECOY"
  REG=$DIR/$CASE.children.json
  "$BIN" regfake -reg "$REG" -pid $DECOY -kind wrongstart
  "$BIN" regfake -reg "$REG" -pid $DECOY -kind wrongimage
  "$BIN" regfake -reg "$REG" -pid $DECOY -kind dead
  cat "$REG"
  "$BIN" reap -reg "$REG"
  sleep 0.5
  n=0; for p in $SURV; do alive $p && n=$((n+1)); done
  echo "after reap: orphans alive = $n ; decoy alive = $(alive $DECOY && echo true || echo false)"
  kill -9 $DECOY; wait $DECOY 2>/dev/null; echo "cleanup: decoy $DECOY killed by pid"
fi
for p in $SURV; do if alive $p; then kill -9 $p; echo "cleanup: killed survivor pid $p"; fi; done
