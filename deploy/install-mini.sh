#!/bin/bash
# Build for the mini (darwin/arm64), copy the binary and the launchd job, (re)load it, and check /healthz and /health.
#   deploy/install-mini.sh            install or upgrade and start (KeepAlive: it runs 24/7)
#   deploy/install-mini.sh --stop     unload the job (the link and the store stay on disk)
# Listens on the mini's loopback and its tailnet address only.
# The switches WA_SEND, WA_READ_RECEIPTS and WA_AGENT_CHATS carry over from the running job (else the installed plist,
# else 0/0/none), so a deploy never silently flips sending. Override one by setting it: WA_SEND=0 deploy/install-mini.sh
# (WA_AGENT_CHATS= with nothing unbinds the agent chat). On the laptop the mini is WA_HOST=shaans-mac-mini.
set -euo pipefail
cd "$(dirname "$0")/.."
HOST=${WA_HOST:-mac-mini}
SSH=(ssh -o ConnectTimeout=10 -o RequestTTY=no -o RemoteCommand=none "$HOST")
LABEL=com.siso.whatsapp-link
if [ "${1:-}" = "--stop" ]; then
  "${SSH[@]}" "launchctl bootout gui/\$(id -u)/$LABEL 2>/dev/null; echo stopped"
  exit 0
fi
TSIP=$("${SSH[@]}" '/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4 2>/dev/null | head -1')
[ -n "$TSIP" ] || { echo "no tailnet address on $HOST" >&2; exit 1; }

# The live switches: the running job's environment block, else the plist on disk. Values only, no secrets live there.
LIVE=$("${SSH[@]}" bash -s <<'REMOTE'
L=com.siso.whatsapp-link; P="$HOME/Library/LaunchAgents/$L.plist"
if launchctl print "gui/$(id -u)/$L" >/dev/null 2>&1; then
  echo "source=running"
  launchctl print "gui/$(id -u)/$L" | awk '
    /^\tenvironment = [{]/ { e = 1; next }
    e && /^\t[}]/ { e = 0 }
    e && ($1 == "WA_SEND" || $1 == "WA_READ_RECEIPTS" || $1 == "WA_AGENT_CHATS") && $2 == "=>" { print $1 "=" $3 }'
elif [ -f "$P" ]; then
  echo "source=plist"
  for k in WA_SEND WA_READ_RECEIPTS WA_AGENT_CHATS; do
    v=$(/usr/libexec/PlistBuddy -c "Print :EnvironmentVariables:$k" "$P" 2>/dev/null) && echo "$k=$v"
  done
else
  echo "source=none"
fi
REMOTE
)
live() { printf '%s\n' "$LIVE" | sed -n "s/^$1=//p" | head -1; }
SRC=$(live source)
SEND=${WA_SEND-$(live WA_SEND)}; SEND=${SEND:-0}
RR=${WA_READ_RECEIPTS-$(live WA_READ_RECEIPTS)}; RR=${RR:-0}
AGENT=${WA_AGENT_CHATS-$(live WA_AGENT_CHATS)}
for v in "$SEND" "$RR"; do
  case "$v" in 0|1) ;; *) echo "WA_SEND and WA_READ_RECEIPTS must be 0 or 1 (got '$v')" >&2; exit 1 ;; esac
done
[[ "$AGENT" =~ ^[0-9A-Za-z@.,_:-]*$ ]] || { echo "WA_AGENT_CHATS must be jids, comma-separated" >&2; exit 1; }
echo "switches (from $SRC unless set by you): WA_SEND=$SEND WA_READ_RECEIPTS=$RR WA_AGENT_CHATS=${AGENT:-none}"

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/siso-whatsapp-link" .
RHOME=$("${SSH[@]}" 'echo $HOME')
sed -e "s#__HOME__#$RHOME#g" -e "s#__LISTEN__#127.0.0.1:5480,$TSIP:5480#" \
  -e "s#__WA_SEND__#$SEND#" -e "s#__WA_READ_RECEIPTS__#$RR#" -e "s#__WA_AGENT_CHATS__#$AGENT#" \
  deploy/com.siso.whatsapp-link.plist > "$tmp/$LABEL.plist"
"${SSH[@]}" 'mkdir -p ~/.local/siso-whatsapp-link ~/Library/LaunchAgents && chmod 700 ~/.local/siso-whatsapp-link'
scp -q -o RequestTTY=no -o RemoteCommand=none "$tmp/siso-whatsapp-link" "$HOST:.local/siso-whatsapp-link/siso-whatsapp-link.new"
scp -q -o RequestTTY=no -o RemoteCommand=none "$tmp/$LABEL.plist" "$HOST:Library/LaunchAgents/$LABEL.plist"
"${SSH[@]}" "set -e; cd ~/.local/siso-whatsapp-link; mv siso-whatsapp-link.new siso-whatsapp-link; \
  launchctl bootout gui/\$(id -u)/$LABEL 2>/dev/null || true; sleep 1; \
  launchctl bootstrap gui/\$(id -u) ~/Library/LaunchAgents/$LABEL.plist; sleep 3; \
  curl -sf -m 5 http://127.0.0.1:5480/healthz && launchctl print gui/\$(id -u)/$LABEL | grep -E '^\s*(state|pid) |WA_'"

# The link must come back signed in from session.db. If it does not, stop: re-pairing is Shaan's QR scan, not ours.
"${SSH[@]}" bash -s <<'REMOTE'
T=$(cat "$HOME/Library/Application Support/siso-whatsapp-link/api-token")
for i in $(seq 1 15); do
  s=$(curl -sf -m 5 -H "Authorization: Bearer $T" http://127.0.0.1:5480/health \
    | jq -c '{version, state: .link.state, loggedIn: .link.loggedIn, connected: .link.connected, sendEnabled, agentChats}')
  case "$s" in *'"loggedIn":true'*'"connected":true'*) echo "health: $s"; exit 0 ;; esac
  sleep 2
done
echo "BLOCKED: the link is not signed in and connected after the restart (health: ${s:-no answer}); do not re-pair from here" >&2
exit 1
REMOTE
echo "installed on $HOST, tailnet http://$TSIP:5480"
