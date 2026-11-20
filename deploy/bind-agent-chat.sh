#!/bin/bash
# Bind the agent chat: the one WhatsApp group SISO agents may read and post in with the agent token.
#   deploy/bind-agent-chat.sh "SISO × Đông A"             look it up, write WA_AGENT_CHATS, reload the job
#   deploy/bind-agent-chat.sh --dry-run "SISO × Đông A"   look it up only
# The lookup runs on the mini with the API token and keeps only groups whose name matches exactly: it prints
# "<jid> <name>" for that one match and fails on none or on more than one. No other chat name or text leaves the mini.
# On the laptop the mini is WA_HOST=shaans-mac-mini.
set -euo pipefail
DRY=0
[ "${1:-}" = "--dry-run" ] && { DRY=1; shift; }
NAME=${1:?usage: deploy/bind-agent-chat.sh [--dry-run] "<exact group name>"}
HOST=${WA_HOST:-mac-mini}
B64=$(printf %s "$NAME" | base64 | tr -d '\n') # the name crosses ssh as base64: no quoting surprises with × or Đ
ssh -o ConnectTimeout=10 -o RequestTTY=no -o RemoteCommand=none "$HOST" bash -s -- "$B64" "$DRY" <<'REMOTE'
set -euo pipefail
NAME=$(printf %s "$1" | base64 -D); DRY=$2
D="$HOME/Library/Application Support/siso-whatsapp-link"
L=com.siso.whatsapp-link; P="$HOME/Library/LaunchAgents/$L.plist"
T=$(cat "$D/api-token")
M=""; off=0
while :; do
  page=$(curl -sf -m 30 -H "Authorization: Bearer $T" "http://127.0.0.1:5480/chats?archived=1&limit=2000&offset=$off") \
    || { echo "the gateway did not answer /chats" >&2; exit 1; }
  M+=$(printf '%s' "$page" | jq -r --arg n "$NAME" '.chats[] | select(.isGroup and .name == $n) | .jid')$'\n'
  [ "$(printf '%s' "$page" | jq '.chats | length')" -lt 2000 ] && break
  off=$((off + 2000))
done
page=""
M=$(printf '%s' "$M" | grep . || true)
n=$(printf '%s' "$M" | grep -c . || true)
[ "$n" -eq 1 ] || { echo "found $n groups named exactly \"$NAME\"; need exactly 1" >&2; exit 1; }
JID=$M
[[ "$JID" =~ ^[0-9-]+@g\.us$ ]] || { echo "the match is not a group jid" >&2; exit 1; }
echo "$JID $NAME"
[ "$DRY" = 1 ] && exit 0
/usr/libexec/PlistBuddy -c "Set :EnvironmentVariables:WA_AGENT_CHATS $JID" "$P" 2>/dev/null \
  || /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:WA_AGENT_CHATS string $JID" "$P"
launchctl bootout "gui/$(id -u)/$L" 2>/dev/null || true; sleep 1
launchctl bootstrap "gui/$(id -u)" "$P"; sleep 3
curl -sf -m 5 http://127.0.0.1:5480/healthz >/dev/null || { echo "healthz failed after reload" >&2; exit 1; }
launchctl print "gui/$(id -u)/$L" | grep -E "WA_(SEND|AGENT_CHATS) "
launchctl print "gui/$(id -u)/$L" | grep -q "WA_AGENT_CHATS => $JID" || { echo "the job did not pick up WA_AGENT_CHATS" >&2; exit 1; }
echo "bound: agents may now read and post (labelled) in $JID only"
REMOTE
