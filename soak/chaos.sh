#!/bin/bash
# Break the gateway on the mini one way and time how long it takes to come back to the state it was in.
#   soak/chaos.sh kill9               kill -9 the process; launchd restarts it
#   soak/chaos.sh restart             launchctl kickstart -k (a clean restart)
#   soak/chaos.sh stall [SECS=60]     freeze the process (SIGSTOP), then thaw it: a hung box or a laptop-lid sleep
#   soak/chaos.sh netdrop [SECS=90]   drop all traffic to WhatsApp's servers with a pf anchor, then lift it
#   soak/chaos.sh all                 the four in turn (netdrop at 90 s and 240 s: past whatsmeow's 3 min keepalive limit)
# Appends one JSON line per test to soak/results/chaos.jsonl: timings, states and counts only. Never sends anything.
# The pf block has a detached failsafe on the mini that lifts it SECS+120 s later whatever happens to this script.
set -euo pipefail
cd "$(dirname "$0")"
HOST=${WA_HOST:-mac-mini}
mkdir -p results
run() {
  ssh -o ConnectTimeout=10 -o RequestTTY=no -o RemoteCommand=none "$HOST" "bash -s -- $1 ${2:-0}" <<'REMOTE' | tee -a results/chaos.jsonl
set -u
TEST=$1 SECS=$2
LABEL=com.siso.whatsapp-link DOM=gui/$(id -u)/com.siso.whatsapp-link
TOKF="$HOME/Library/Application Support/siso-whatsapp-link/api-token"
ANCHOR=com.apple/siso-wa-soak
# Meta's WhatsApp edge ranges (AS32934), plus whatever the process is connected to right now.
NETS="157.240.0.0/16 31.13.24.0/21 31.13.64.0/18 163.70.128.0/17 129.134.0.0/16 179.60.192.0/22 185.60.216.0/22 69.171.224.0/19 66.220.144.0/20 102.132.96.0/20 173.252.64.0/18 2a03:2880::/32"
now() { perl -MTime::HiRes=time -e 'printf "%.1f\n", time'; }
since() { perl -e "printf '%.1f', $(now) - $1"; }
# "state loggedIn connected reconnects pid" from the gateway itself, or "down" if it does not answer in 2 s.
st() {
  local h; h=$(curl -s -m 2 -H "Authorization: Bearer $(cat "$TOKF")" http://127.0.0.1:5480/health 2>/dev/null) || { echo down; return; }
  echo "$h" | jq -r '"\(.link.state) \(.link.loggedIn) \(.link.connected) \(.link.reconnects)"' 2>/dev/null || echo down
}
pid() { launchctl print "$DOM" 2>/dev/null | awk '/^\tpid = /{print $3}'; }
lift() { sudo -n pfctl -a "$ANCHOR" -F all >/dev/null 2>&1; [ -n "${PFTOK:-}" ] && sudo -n pfctl -X "$PFTOK" >/dev/null 2>&1; PFTOK=; }

read -r S0 L0 C0 R0 <<<"$(st)"
P0=$(pid)
if [ "$S0" != connected ] && [ "$S0" != needs_qr ]; then
  printf '{"test":"%s","at":%s,"ok":false,"why":"not steady before the test","before":"%s"}\n' "$TEST" "$(date +%s)" "$S0"; exit 0
fi
T0=$(now) DET=null UNDO=null PFTOK= BLOCKED=null
case $TEST in
  kill9) kill -9 "$P0" ;;
  restart) launchctl kickstart -k "$DOM" >/dev/null ;;
  stall)
    kill -STOP "$P0"; trap 'kill -CONT '"$P0"' 2>/dev/null' EXIT
    nohup bash -c "sleep $((SECS + 60)); kill -CONT $P0" >/dev/null 2>&1 &
    ;;
  netdrop)
    IPS=$(lsof -nP -a -p "$P0" -iTCP -sTCP:ESTABLISHED -Fn 2>/dev/null | sed -n 's/^n.*->//p' | sed -E 's/:[0-9]+$//; s/^\[//; s/\]$//' | grep -Ev '^(100\.|127\.|::1$)' | sort -u | tr '\n' ' ')
    trap lift EXIT
    printf 'table <wa> { %s }\nblock drop quick proto tcp from any to <wa>\nblock drop quick proto tcp from <wa> to any\n' "$NETS $IPS" | sudo -n pfctl -a "$ANCHOR" -f - 2>/dev/null
    PFTOK=$(sudo -n pfctl -E 2>&1 | sed -n 's/^Token : //p')
    for n in $NETS $IPS; do sudo -n pfctl -k 0.0.0.0/0 -k "$n"; sudo -n pfctl -k "$n"; done >/dev/null 2>&1
    nohup bash -c "sleep $((SECS + 120)); sudo -n pfctl -a $ANCHOR -F all; sudo -n pfctl -X $PFTOK" >/dev/null 2>&1 &
    # Prove the break took effect: WhatsApp's web endpoint must be unreachable from the mini now.
    curl -s -m 5 -o /dev/null https://web.whatsapp.com && BLOCKED=false || BLOCKED=true
    ;;
  *) echo "{\"test\":\"$TEST\",\"ok\":false,\"why\":\"unknown test\"}"; exit 0 ;;
esac
# Watch: when the gateway first shows the break, when the break is undone, and when it is back where it started.
BACK=null
while :; do
  E=$(since "$T0")
  if [ "$UNDO" = null ] && [ "$TEST" = stall -o "$TEST" = netdrop ] && perl -e "exit !($E >= $SECS)"; then
    [ "$TEST" = stall ] && kill -CONT "$P0" && trap - EXIT
    [ "$TEST" = netdrop ] && lift && trap - EXIT
    UNDO=$E
  fi
  read -r S L C R <<<"$(st)"
  if [ "$TEST" != stall ] || [ "$UNDO" != null ]; then
    if [ "$DET" = null ] && { [ "$S" != "$S0" ] || [ "$C" != "$C0" ]; }; then DET=$E; fi
  elif [ "$DET" = null ] && [ "$S" = down ]; then DET=$E; fi
  if { [ "$TEST" = kill9 ] || [ "$TEST" = restart ] || [ "$UNDO" != null ]; } && [ "$S" = "$S0" ] && [ "$C" = "$C0" ] && [ "$L" = "$L0" ]; then
    if [ "$TEST" != kill9 ] && [ "$TEST" != restart ] || [ "$(pid)" != "$P0" ]; then BACK=$E; break; fi
  fi
  perl -e "exit !($E > $SECS + 300)" && break
  sleep 0.5
done
P1=$(pid)
AFTER=$( [ "$UNDO" = null ] && echo null || perl -e "printf '%.1f', ${BACK/null/0} - $UNDO" )
[ "$BACK" = null ] && AFTER=null
printf '{"test":"%s","secs":%s,"at":%s,"before":"%s","linked":%s,"detectedSec":%s,"undoneSec":%s,"backSec":%s,"backAfterUndoSec":%s,"blockVerified":%s,"ok":%s,"pidChanged":%s,"reconnects":%s,"after":"%s"}\n' \
  "$TEST" "$SECS" "${T0%.*}" "$S0" "$L0" "$DET" "$UNDO" "$BACK" "$AFTER" "$BLOCKED" "$( [ "$BACK" != null ] && [ "$BLOCKED" != false ] && echo true || echo false )" \
  "$( [ "$P1" != "$P0" ] && echo true || echo false )" "$( [ "$P1" != "$P0" ] && echo "${R:-0}" || echo $(( ${R:-0} - ${R0:-0} )) )" "$S"
REMOTE
}
case ${1:-} in
  all) run kill9; sleep 30; run restart; sleep 30; run stall 60; sleep 30; run netdrop 90; sleep 30; run netdrop 240 ;;
  kill9|restart) run "$1" ;;
  stall) run stall "${2:-60}" ;;
  netdrop) run netdrop "${2:-90}" ;;
  *) sed -n 2,9p "$0"; exit 2 ;;
esac
